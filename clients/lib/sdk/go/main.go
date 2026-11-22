// The verbs of defs/sdk-go.yml, on the AWS SDK for Go v2.
package main

import (
	"context"
	"fmt"
	"log"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/config"
	"github.com/aws/aws-sdk-go-v2/credentials"
	"github.com/aws/aws-sdk-go-v2/feature/s3/manager"
	"github.com/aws/aws-sdk-go-v2/service/s3"
)

func main() {
	if err := run(context.Background(), os.Args[1], os.Args[2:]); err != nil {
		log.Fatal(err)
	}
}

func run(ctx context.Context, verb string, args []string) error {
	cfg, err := config.LoadDefaultConfig(ctx,
		config.WithRegion("us-east-1"),
		config.WithCredentialsProvider(credentials.NewStaticCredentialsProvider(
			os.Getenv("ACCESS_KEY"), os.Getenv("SECRET_KEY"), "")))
	if err != nil {
		return err
	}
	client := s3.NewFromConfig(cfg, func(o *s3.Options) {
		o.BaseEndpoint = aws.String(os.Getenv("ENDPOINT"))
		o.UsePathStyle = true
	})
	bucket := aws.String(os.Getenv("BUCKET"))

	// the transfer manager, so a large file goes up in parts
	upload := func(file, key string) error {
		f, err := os.Open(file)
		if err != nil {
			return err
		}
		defer f.Close()
		_, err = manager.NewUploader(client).Upload(ctx, &s3.PutObjectInput{Bucket: bucket, Key: &key, Body: f})
		return err
	}

	switch verb {
	case "put":
		return upload(args[0], args[1])
	case "get":
		f, err := os.Create(args[1])
		if err != nil {
			return err
		}
		defer f.Close()
		_, err = manager.NewDownloader(client).Download(ctx, f, &s3.GetObjectInput{Bucket: bucket, Key: &args[0]})
		return err
	case "list":
		p := s3.NewListObjectsV2Paginator(client, &s3.ListObjectsV2Input{Bucket: bucket, Prefix: aws.String(args[0] + "/")})
		for p.HasMorePages() {
			page, err := p.NextPage(ctx)
			if err != nil {
				return err
			}
			for _, object := range page.Contents {
				fmt.Println(*object.Key)
			}
		}
		return nil
	case "delete":
		_, err := client.DeleteObject(ctx, &s3.DeleteObjectInput{Bucket: bucket, Key: &args[0]})
		return err
	case "copy":
		segments := strings.Split(args[0], "/")
		for i, s := range segments {
			segments[i] = url.PathEscape(s)
		}
		_, err := client.CopyObject(ctx, &s3.CopyObjectInput{
			Bucket: bucket, Key: &args[1],
			CopySource: aws.String(*bucket + "/" + strings.Join(segments, "/")),
		})
		return err
	case "presign":
		req, err := s3.NewPresignClient(client).PresignGetObject(ctx,
			&s3.GetObjectInput{Bucket: bucket, Key: &args[0]}, s3.WithPresignExpires(time.Hour))
		if err != nil {
			return err
		}
		fmt.Println(req.URL)
		return nil
	case "lifecycle":
		out, err := client.GetBucketLifecycleConfiguration(ctx, &s3.GetBucketLifecycleConfigurationInput{Bucket: bucket})
		if err != nil {
			return err
		}
		for _, rule := range out.Rules {
			prefix, expire, abort := "", "-", "-"
			if rule.Filter != nil {
				prefix = aws.ToString(rule.Filter.Prefix)
			}
			if rule.Expiration != nil && rule.Expiration.Days != nil {
				expire = fmt.Sprint(*rule.Expiration.Days)
			}
			if a := rule.AbortIncompleteMultipartUpload; a != nil && a.DaysAfterInitiation != nil {
				abort = fmt.Sprint(*a.DaysAfterInitiation)
			}
			fmt.Printf("%s prefix=%s expire=%s abort=%s\n", aws.ToString(rule.ID), prefix, expire, abort)
		}
		return nil
	case "replication":
		out, err := client.GetBucketReplication(ctx, &s3.GetBucketReplicationInput{Bucket: bucket})
		if err != nil {
			return err
		}
		for _, rule := range out.ReplicationConfiguration.Rules {
			prefix, destination := "", ""
			if rule.Filter != nil {
				prefix = aws.ToString(rule.Filter.Prefix)
			}
			if rule.Destination != nil {
				destination = aws.ToString(rule.Destination.Bucket)
			}
			fmt.Printf("%s prefix=%s status=%s destination=%s\n", aws.ToString(rule.ID), prefix, rule.Status, destination)
		}
		return nil
	case "tags":
		out, err := client.GetBucketTagging(ctx, &s3.GetBucketTaggingInput{Bucket: bucket})
		if err != nil {
			return err
		}
		for _, tag := range out.TagSet {
			fmt.Printf("%s=%s\n", aws.ToString(tag.Key), aws.ToString(tag.Value))
		}
		return nil
	case "notification":
		out, err := client.GetBucketNotificationConfiguration(ctx, &s3.GetBucketNotificationConfigurationInput{Bucket: bucket})
		if err != nil {
			return err
		}
		for _, topic := range out.TopicConfigurations {
			events := make([]string, len(topic.Events))
			for i, event := range topic.Events {
				events[i] = string(event)
			}
			fmt.Printf("%s topic=%s events=%s\n", aws.ToString(topic.Id), aws.ToString(topic.TopicArn), strings.Join(events, ","))
		}
		return nil
	case "logging":
		out, err := client.GetBucketLogging(ctx, &s3.GetBucketLoggingInput{Bucket: bucket})
		if err != nil {
			return err
		}
		target, prefix := "", ""
		if enabled := out.LoggingEnabled; enabled != nil {
			target, prefix = aws.ToString(enabled.TargetBucket), aws.ToString(enabled.TargetPrefix)
		}
		fmt.Printf("target=%s prefix=%s\n", target, prefix)
		return nil
	case "put_tree":
		entries, err := os.ReadDir(args[0])
		if err != nil {
			return err
		}
		for _, e := range entries {
			if err := upload(filepath.Join(args[0], e.Name()), args[1]+"/"+e.Name()); err != nil {
				return err
			}
		}
		return nil
	}
	return fmt.Errorf("unknown verb %s", verb)
}
