// SPDX-License-Identifier: GPL-3.0-or-later
package awsops

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/service/cloudfront"
	cftypes "github.com/aws/aws-sdk-go-v2/service/cloudfront/types"
	"github.com/aws/aws-sdk-go-v2/service/s3"
	s3types "github.com/aws/aws-sdk-go-v2/service/s3/types"
)

// Upload puts one file in the bucket.
//
// ContentType is set explicitly because S3 guesses from nothing and CloudFront
// passes the guess straight through: an appcast served as
// application/octet-stream is one some XML parsers refuse.
func (c *Client) Upload(ctx context.Context, bucket, key, path, contentType, cacheControl string) error {
	file, err := os.Open(path)
	if err != nil {
		return err
	}
	defer file.Close()

	info, err := file.Stat()
	if err != nil {
		return err
	}

	_, err = c.S3.PutObject(ctx, &s3.PutObjectInput{
		Bucket:        aws.String(bucket),
		Key:           aws.String(key),
		Body:          file,
		ContentLength: aws.Int64(info.Size()),
		ContentType:   aws.String(contentType),
		CacheControl:  aws.String(cacheControl),
	})
	if err != nil {
		return fmt.Errorf("uploading %s to s3://%s/%s: %w", filepath.Base(path), bucket, key, err)
	}
	return nil
}

// ObjectExists is how a release avoids overwriting an archive a published feed
// already points at. Versioning would make that recoverable; not doing it at
// all is better.
func (c *Client) ObjectExists(ctx context.Context, bucket, key string) (bool, error) {
	_, err := c.S3.HeadObject(ctx, &s3.HeadObjectInput{
		Bucket: aws.String(bucket),
		Key:    aws.String(key),
	})
	if err == nil {
		return true, nil
	}
	if isNotFound(err) {
		return false, nil
	}
	return false, err
}

func isNotFound(err error) bool {
	var notFound *s3types.NotFound
	if errors.As(err, &notFound) {
		return true
	}
	var noSuchKey *s3types.NoSuchKey
	return errors.As(err, &noSuchKey)
}

// Invalidate clears the cached appcast so a release is visible now rather than
// whenever the edge decides.
//
// Only the feed: the archives are immutable — their names carry the version —
// so invalidating them would throw away a cache that is always correct. The
// call returns as soon as CloudFront accepts it; waiting is the caller's
// choice, because it takes a minute and nothing depends on it having finished.
func (c *Client) Invalidate(ctx context.Context, distributionID string, paths ...string) (string, error) {
	if len(paths) == 0 {
		paths = []string{"/appcast.xml"}
	}

	out, err := c.CloudFront.CreateInvalidation(ctx, &cloudfront.CreateInvalidationInput{
		DistributionId: aws.String(distributionID),
		InvalidationBatch: &cftypes.InvalidationBatch{
			// Unique per call, and CloudFront treats a repeated reference as a
			// request for the invalidation it already has.
			CallerReference: aws.String(fmt.Sprintf("publish-%d", time.Now().UnixNano())),
			Paths: &cftypes.Paths{
				Quantity: aws.Int32(int32(len(paths))),
				Items:    paths,
			},
		},
	})
	if err != nil {
		return "", fmt.Errorf("invalidating %v on %s: %w", paths, distributionID, err)
	}
	return aws.ToString(out.Invalidation.Id), nil
}
