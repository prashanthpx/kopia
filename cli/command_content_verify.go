package cli

import (
	"context"
	"sync/atomic"

	"github.com/pkg/errors"

	"github.com/kopia/kopia/repo"
	"github.com/kopia/kopia/repo/blob"
	"github.com/kopia/kopia/repo/content"
)

type commandContentVerify struct {
	contentVerifyParallel       int
	contentVerifyFull           bool
	contentVerifyIncludeDeleted bool

	contentRange contentRangeFlags
}

func (c *commandContentVerify) setup(svc appServices, parent commandParent) {
	cmd := parent.Command("verify", "Verify that each content is backed by a valid blob")

	cmd.Flag("parallel", "Parallelism").Default("16").IntVar(&c.contentVerifyParallel)
	cmd.Flag("full", "Full verification (including download)").BoolVar(&c.contentVerifyFull)
	cmd.Flag("include-deleted", "Include deleted contents").BoolVar(&c.contentVerifyIncludeDeleted)
	c.contentRange.setup(cmd)
	cmd.Action(svc.directRepositoryReadAction(c.run))
}

func readBlobMap(ctx context.Context, br blob.Reader) (map[blob.ID]blob.Metadata, error) {
	blobMap := map[blob.ID]blob.Metadata{}

	log(ctx).Infof("Listing blobs...")

	if err := br.ListBlobs(ctx, "", func(bm blob.Metadata) error {
		blobMap[bm.BlobID] = bm
		if len(blobMap)%10000 == 0 {
			log(ctx).Infof("  %v blobs...", len(blobMap))
		}
		return nil
	}); err != nil {
		return nil, errors.Wrap(err, "unable to list blobs")
	}

	log(ctx).Infof("Listed %v blobs.", len(blobMap))

	return blobMap, nil
}

func (c *commandContentVerify) run(ctx context.Context, rep repo.DirectRepository) error {
	blobMap := map[blob.ID]blob.Metadata{}

	if !c.contentVerifyFull {
		m, err := readBlobMap(ctx, rep.BlobReader())
		if err != nil {
			return err
		}

		blobMap = m
	}

	var totalCount, successCount, errorCount int32

	log(ctx).Infof("Verifying all contents...")

	err := rep.ContentReader().IterateContents(ctx, content.IterateOptions{
		Range:          c.contentRange.contentIDRange(),
		Parallel:       c.contentVerifyParallel,
		IncludeDeleted: c.contentVerifyIncludeDeleted,
	}, func(ci content.Info) error {
		if err := c.contentVerify(ctx, rep.ContentReader(), ci, blobMap); err != nil {
			log(ctx).Errorf("error %v", err)
			atomic.AddInt32(&errorCount, 1)
		} else {
			atomic.AddInt32(&successCount, 1)
		}

		if t := atomic.AddInt32(&totalCount, 1); t%100000 == 0 {
			log(ctx).Infof("  %v contents, %v errors...", t, atomic.LoadInt32(&errorCount))
		}

		return nil
	})
	if err != nil {
		return errors.Wrap(err, "iterate contents")
	}

	log(ctx).Infof("Finished verifying %v contents, found %v errors.", totalCount, errorCount)

	if errorCount == 0 {
		return nil
	}

	return errors.Errorf("encountered %v errors", errorCount)
}

func (c *commandContentVerify) contentVerify(ctx context.Context, r content.Reader, ci content.Info, blobMap map[blob.ID]blob.Metadata) error {
	if c.contentVerifyFull {
		if _, err := r.GetContent(ctx, ci.GetContentID()); err != nil {
			return errors.Wrapf(err, "content %v is invalid", ci.GetContentID())
		}

		return nil
	}

	bi, ok := blobMap[ci.GetPackBlobID()]
	if !ok {
		return errors.Errorf("content %v depends on missing blob %v", ci.GetContentID(), ci.GetPackBlobID())
	}

	if int64(ci.GetPackOffset()+ci.GetPackedLength()) > bi.Length {
		return errors.Errorf("content %v out of bounds of its pack blob %v", ci.GetContentID(), ci.GetPackBlobID())
	}

	return nil
}
