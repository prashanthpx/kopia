// Package cli implements command-line commands for the Kopia.
package cli

import (
	"context"
	"net/http"
	"os"
	"time"

	"github.com/alecthomas/kingpin"
	"github.com/fatih/color"
	"github.com/pkg/errors"

	"github.com/kopia/kopia/internal/apiclient"
	"github.com/kopia/kopia/repo"
	"github.com/kopia/kopia/repo/blob"
	"github.com/kopia/kopia/repo/logging"
	"github.com/kopia/kopia/repo/maintenance"
	"github.com/kopia/kopia/snapshot/snapshotmaintenance"
)

var log = logging.GetContextLoggerFunc("kopia/cli")

var (
	defaultColor = color.New()
	warningColor = color.New(color.FgYellow)
	errorColor   = color.New(color.FgHiRed)
)

// appServices are the methods of *App that command handles are allowed to call.
type appServices interface {
	noRepositoryAction(act func(ctx context.Context) error) func(ctx *kingpin.ParseContext) error
	serverAction(sf *serverClientFlags, act func(ctx context.Context, cli *apiclient.KopiaAPIClient) error) func(ctx *kingpin.ParseContext) error
	directRepositoryWriteAction(act func(ctx context.Context, rep repo.DirectRepositoryWriter) error) func(ctx *kingpin.ParseContext) error
	directRepositoryReadAction(act func(ctx context.Context, rep repo.DirectRepository) error) func(ctx *kingpin.ParseContext) error
	repositoryReaderAction(act func(ctx context.Context, rep repo.Repository) error) func(ctx *kingpin.ParseContext) error
	repositoryWriterAction(act func(ctx context.Context, rep repo.RepositoryWriter) error) func(ctx *kingpin.ParseContext) error
	maybeRepositoryAction(act func(ctx context.Context, rep repo.Repository) error, mode repositoryAccessMode) func(ctx *kingpin.ParseContext) error

	repositoryConfigFileName() string
	getProgress() *cliProgress
}

type advancedAppServices interface {
	appServices

	runConnectCommandWithStorage(ctx context.Context, co *connectOptions, st blob.Storage) error
	runConnectCommandWithStorageAndPassword(ctx context.Context, co *connectOptions, st blob.Storage, password string) error
	openRepository(ctx context.Context, required bool) (repo.Repository, error)

	maybeInitializeUpdateCheck(ctx context.Context, co *connectOptions)
	removeUpdateState()
	getPasswordFromFlags(ctx context.Context, isNew, allowPersistent bool) (string, error)
	optionsFromFlags(ctx context.Context) *repo.Options
}

// App contains per-invocation flags and state of Kopia CLI.
type App struct {
	// global flags
	enableAutomaticMaintenance    bool
	mt                            memoryTracker
	progress                      *cliProgress
	initialUpdateCheckDelay       time.Duration
	updateCheckInterval           time.Duration
	updateAvailableNotifyInterval time.Duration
	configPath                    string
	traceStorage                  bool
	metricsListenAddr             string

	// subcommands
	blob        commandBlob
	benchmark   commandBenchmark
	cache       commandCache
	content     commandContent
	diff        commandDiff
	index       commandIndex
	list        commandList
	server      commandServer
	session     commandSession
	policy      commandPolicy
	restore     commandRestore
	show        commandShow
	snapshot    commandSnapshot
	manifest    commandManifest
	mount       commandMount
	maintenance commandMaintenance
	repository  commandRepository
}

func (c *App) getProgress() *cliProgress {
	return c.progress
}

func (c *App) setup(app *kingpin.Application) {
	_ = app.Flag("help-full", "Show help for all commands, including hidden").Action(func(pc *kingpin.ParseContext) error {
		_ = app.UsageForContextWithTemplate(pc, 0, kingpin.DefaultUsageTemplate)
		os.Exit(0)
		return nil
	}).Bool()

	app.Flag("auto-maintenance", "Automatic maintenance").Default("true").Hidden().BoolVar(&c.enableAutomaticMaintenance)

	// hidden flags to control auto-update behavior.
	app.Flag("initial-update-check-delay", "Initial delay before first time update check").Default("24h").Hidden().Envar("KOPIA_INITIAL_UPDATE_CHECK_DELAY").DurationVar(&c.initialUpdateCheckDelay)
	app.Flag("update-check-interval", "Interval between update checks").Default("168h").Hidden().Envar("KOPIA_UPDATE_CHECK_INTERVAL").DurationVar(&c.updateCheckInterval)
	app.Flag("update-available-notify-interval", "Interval between update notifications").Default("1h").Hidden().Envar("KOPIA_UPDATE_NOTIFY_INTERVAL").DurationVar(&c.updateAvailableNotifyInterval)
	app.Flag("config-file", "Specify the config file to use.").Default(defaultConfigFileName()).Envar("KOPIA_CONFIG_PATH").StringVar(&c.configPath)
	app.Flag("trace-storage", "Enables tracing of storage operations.").Default("true").Hidden().BoolVar(&c.traceStorage)
	app.Flag("metrics-listen-addr", "Expose Prometheus metrics on a given host:port").Hidden().StringVar(&c.metricsListenAddr)
	app.Flag("timezone", "Format time according to specified time zone (local, utc, original or time zone name)").Default("local").Hidden().StringVar(&timeZone)
	app.Flag("password", "Repository password.").Envar("KOPIA_PASSWORD").Short('p').StringVar(&globalPassword)

	c.setupOSSpecificKeychainFlags(app)

	_ = app.Flag("caching", "Enables caching of objects (disable with --no-caching)").Default("true").Hidden().Action(
		deprecatedFlag("The '--caching' flag is deprecated and has no effect, use 'kopia cache set' instead."),
	).Bool()

	_ = app.Flag("list-caching", "Enables caching of list results (disable with --no-list-caching)").Default("true").Hidden().Action(
		deprecatedFlag("The '--list-caching' flag is deprecated and has no effect, use 'kopia cache set' instead."),
	).Bool()

	c.mt.setup(app)
	c.progress.setup(app)

	c.blob.setup(c, app)
	c.benchmark.setup(c, app)
	c.cache.setup(c, app)
	c.content.setup(c, app)
	c.diff.setup(c, app)
	c.index.setup(c, app)
	c.list.setup(c, app)
	c.server.setup(c, app)
	c.session.setup(c, app)
	c.restore.setup(c, app)
	c.show.setup(c, app)
	c.snapshot.setup(c, app)
	c.manifest.setup(c, app)
	c.policy.setup(c, app)
	c.mount.setup(c, app)
	c.maintenance.setup(c, app)
	c.repository.setup(c, app)
}

// commandParent is implemented by app and commands that can have sub-commands.
type commandParent interface {
	Command(name, help string) *kingpin.CmdClause
}

// Attach creates new App object and attaches all the flags to the provided application.
func Attach(app *kingpin.Application) *App {
	a := &App{
		progress: &cliProgress{},
	}

	a.setup(app)

	return a
}

var safetyByName = map[string]maintenance.SafetyParameters{
	"none": maintenance.SafetyNone,
	"full": maintenance.SafetyFull,
}

// safetyFlagVar defines c --safety=none|full flag that sets the SafetyParameters.
func safetyFlagVar(cmd *kingpin.CmdClause, result *maintenance.SafetyParameters) {
	var str string

	*result = maintenance.SafetyFull

	cmd.Flag("safety", "Safety level").Default("full").PreAction(func(pc *kingpin.ParseContext) error {
		r, ok := safetyByName[str]
		if !ok {
			return errors.Errorf("unhandled safety level")
		}

		*result = r

		return nil
	}).EnumVar(&str, "full", "none")
}

func (c *App) noRepositoryAction(act func(ctx context.Context) error) func(ctx *kingpin.ParseContext) error {
	return func(_ *kingpin.ParseContext) error {
		return act(rootContext())
	}
}

func (c *App) serverAction(sf *serverClientFlags, act func(ctx context.Context, cli *apiclient.KopiaAPIClient) error) func(ctx *kingpin.ParseContext) error {
	return func(_ *kingpin.ParseContext) error {
		opts, err := sf.serverAPIClientOptions()
		if err != nil {
			return errors.Wrap(err, "unable to create API client options")
		}

		apiClient, err := apiclient.NewKopiaAPIClient(opts)
		if err != nil {
			return errors.Wrap(err, "unable to create API client")
		}

		return act(rootContext(), apiClient)
	}
}

func assertDirectRepository(act func(ctx context.Context, rep repo.DirectRepository) error) func(ctx context.Context, rep repo.Repository) error {
	return func(ctx context.Context, rep repo.Repository) error {
		if rep == nil {
			return act(ctx, nil)
		}

		// right now this assertion never fails,
		// but will fail in the future when we have remote repository implementation
		lr, ok := rep.(repo.DirectRepository)
		if !ok {
			return errors.Errorf("operation supported only on direct repository")
		}

		return act(ctx, lr)
	}
}

func (c *App) directRepositoryWriteAction(act func(ctx context.Context, rep repo.DirectRepositoryWriter) error) func(ctx *kingpin.ParseContext) error {
	return c.maybeRepositoryAction(assertDirectRepository(func(ctx context.Context, rep repo.DirectRepository) error {
		return repo.DirectWriteSession(ctx, rep, repo.WriteSessionOptions{
			Purpose:  "directRepositoryWriteAction",
			OnUpload: c.progress.UploadedBytes,
		}, func(dw repo.DirectRepositoryWriter) error { return act(ctx, dw) })
	}), repositoryAccessMode{
		mustBeConnected:    true,
		disableMaintenance: true,
	})
}

func (c *App) directRepositoryReadAction(act func(ctx context.Context, rep repo.DirectRepository) error) func(ctx *kingpin.ParseContext) error {
	return c.maybeRepositoryAction(assertDirectRepository(func(ctx context.Context, rep repo.DirectRepository) error {
		return act(ctx, rep)
	}), repositoryAccessMode{
		mustBeConnected:    true,
		disableMaintenance: true,
	})
}

func (c *App) repositoryReaderAction(act func(ctx context.Context, rep repo.Repository) error) func(ctx *kingpin.ParseContext) error {
	return c.maybeRepositoryAction(func(ctx context.Context, rep repo.Repository) error {
		return act(ctx, rep)
	}, repositoryAccessMode{
		mustBeConnected:    true,
		disableMaintenance: true,
	})
}

func (c *App) repositoryWriterAction(act func(ctx context.Context, rep repo.RepositoryWriter) error) func(ctx *kingpin.ParseContext) error {
	return c.maybeRepositoryAction(func(ctx context.Context, rep repo.Repository) error {
		return repo.WriteSession(ctx, rep, repo.WriteSessionOptions{
			Purpose:  "repositoryWriterAction",
			OnUpload: c.progress.UploadedBytes,
		}, func(w repo.RepositoryWriter) error {
			return act(ctx, w)
		})
	}, repositoryAccessMode{
		mustBeConnected: true,
	})
}

func rootContext() context.Context {
	return context.Background()
}

type repositoryAccessMode struct {
	mustBeConnected    bool
	disableMaintenance bool
}

func (c *App) maybeRepositoryAction(act func(ctx context.Context, rep repo.Repository) error, mode repositoryAccessMode) func(ctx *kingpin.ParseContext) error {
	return func(kpc *kingpin.ParseContext) error {
		ctx := rootContext()

		if err := withProfiling(func() error {
			c.mt.startMemoryTracking(ctx)
			defer c.mt.finishMemoryTracking(ctx)

			if c.metricsListenAddr != "" {
				mux := http.NewServeMux()
				if err := initPrometheus(mux); err != nil {
					return errors.Wrap(err, "unable to initialize prometheus.")
				}

				log(ctx).Infof("starting prometheus metrics on %v", c.metricsListenAddr)
				go http.ListenAndServe(c.metricsListenAddr, mux) // nolint:errcheck
			}

			rep, err := c.openRepository(ctx, mode.mustBeConnected)
			if err != nil && mode.mustBeConnected {
				return errors.Wrap(err, "open repository")
			}

			err = act(ctx, rep)

			if rep != nil && !mode.disableMaintenance {
				if merr := c.maybeRunMaintenance(ctx, rep); merr != nil {
					log(ctx).Errorf("error running maintenance: %v", merr)
				}
			}

			if rep != nil && mode.mustBeConnected {
				if cerr := rep.Close(ctx); cerr != nil {
					return errors.Wrap(cerr, "unable to close repository")
				}
			}

			return err
		}); err != nil {
			// print error in red
			log(ctx).Errorf("ERROR: %v", err.Error())
			os.Exit(1)
		}

		return nil
	}
}

func (c *App) maybeRunMaintenance(ctx context.Context, rep repo.Repository) error {
	if !c.enableAutomaticMaintenance {
		return nil
	}

	if rep.ClientOptions().ReadOnly {
		return nil
	}

	dr, ok := rep.(repo.DirectRepository)
	if !ok {
		return nil
	}

	err := repo.DirectWriteSession(ctx, dr, repo.WriteSessionOptions{
		Purpose:  "maybeRunMaintenance",
		OnUpload: c.progress.UploadedBytes,
	}, func(w repo.DirectRepositoryWriter) error {
		return snapshotmaintenance.Run(ctx, w, maintenance.ModeAuto, false, maintenance.SafetyFull)
	})

	var noe maintenance.NotOwnedError

	if errors.As(err, &noe) {
		// do not report the NotOwnedError to the user since this is automatic maintenance.
		return nil
	}

	return errors.Wrap(err, "error running maintenance")
}

func advancedCommand(ctx context.Context) {
	if os.Getenv("KOPIA_ADVANCED_COMMANDS") != "enabled" {
		log(ctx).Errorf(`
This command could be dangerous or lead to repository corruption when used improperly.

Running this command is not needed for using Kopia. Instead, most users should rely on periodic repository maintenance. See https://kopia.io/docs/advanced/maintenance/ for more information.
To run this command despite the warning, set KOPIA_ADVANCED_COMMANDS=enabled

`)
		os.Exit(1)
	}
}
