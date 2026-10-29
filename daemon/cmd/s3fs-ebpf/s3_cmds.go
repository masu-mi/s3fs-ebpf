// s3-list / s3-stat subcommands.

package main

import (
	"context"
	"flag"
	"fmt"
	"os"
	"time"
)

// runS3List lists objects in the configured bucket (optionally under a
// prefix) and prints them. Exit 1 on connection or listing failure.
func runS3List(args []string) int {
	fs := flag.NewFlagSet("s3-list", flag.ExitOnError)
	newClient := backendFlags(fs)
	prefix := fs.String("prefix", "", "Object key prefix to enumerate")
	fs.Parse(args)

	client, err := newClient()
	if err != nil {
		fmt.Fprintf(os.Stderr, "s3-list: %v\n", err)
		return 1
	}
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	objs, err := client.List(ctx, *prefix)
	if err != nil {
		fmt.Fprintf(os.Stderr, "s3-list: %v\n", err)
		return 1
	}
	fmt.Printf("bucket=%s prefix=%q count=%d\n", client.Bucket(), *prefix, len(objs))
	for _, o := range objs {
		fmt.Printf("  %s\t%d\t%s\t%s\n",
			o.Key, o.Size, o.ETag, o.LastModified.UTC().Format(time.RFC3339))
	}
	return 0
}

// runS3Stat fetches a single object's HEAD metadata and prints it.
func runS3Stat(args []string) int {
	fs := flag.NewFlagSet("s3-stat", flag.ExitOnError)
	newClient := backendFlags(fs)
	fs.Parse(args)
	if fs.NArg() != 1 {
		fmt.Fprintln(os.Stderr, "s3-stat: exactly one KEY argument required")
		return 2
	}
	key := fs.Arg(0)

	client, err := newClient()
	if err != nil {
		fmt.Fprintf(os.Stderr, "s3-stat: %v\n", err)
		return 1
	}
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	meta, err := client.Stat(ctx, key)
	if err != nil {
		fmt.Fprintf(os.Stderr, "s3-stat: %v\n", err)
		return 1
	}
	fmt.Printf("bucket=%s key=%s\n", client.Bucket(), meta.Key)
	fmt.Printf("  size          %d\n", meta.Size)
	fmt.Printf("  etag          %s\n", meta.ETag)
	fmt.Printf("  content-type  %s\n", meta.ContentType)
	fmt.Printf("  last-modified %s\n", meta.LastModified.UTC().Format(time.RFC3339))
	return 0
}
