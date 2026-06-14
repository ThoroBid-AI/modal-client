package main

import (
	"context"
	"fmt"
	"log"

	modal "github.com/modal-labs/modal-client/go"
)

// This example demonstrates the Volume file-management API: listing, reading,
// copying, and removing files directly from the client, plus committing and
// reloading a Volume. Files are first written by mounting the Volume in a
// Sandbox, then inspected from the client without a Sandbox.
func main() {
	ctx := context.Background()
	mc, err := modal.NewClient()
	if err != nil {
		log.Fatalf("Failed to create client: %v", err)
	}

	app, err := mc.Apps.FromName(ctx, "libmodal-example", &modal.AppFromNameParams{CreateIfMissing: true})
	if err != nil {
		log.Fatalf("Failed to get or create App: %v", err)
	}

	image := mc.Images.FromRegistry("alpine:3.21", nil)

	// The file-management RPCs (ListDir, ReadFile, CopyFiles, ...) require a v2
	// Volume, which is the SDK default when Version is left unspecified.
	volume, err := mc.Volumes.FromName(ctx, "libmodal-example-volume-files-v2", &modal.VolumeFromNameParams{
		CreateIfMissing: true,
	})
	if err != nil {
		log.Fatalf("Failed to create Volume: %v", err)
	}

	// Write some files into the Volume by mounting it in a Sandbox.
	writer, err := mc.Sandboxes.Create(ctx, app, image, &modal.SandboxCreateParams{
		Command: []string{
			"sh", "-c",
			"mkdir -p /mnt/volume/data && " +
				"echo 'hello from modal' > /mnt/volume/data/message.txt && " +
				"echo 'second file' > /mnt/volume/data/other.txt",
		},
		Volumes: map[string]*modal.Volume{"/mnt/volume": volume},
	})
	if err != nil {
		log.Fatalf("Failed to create writer Sandbox: %v", err)
	}
	if _, err := writer.Wait(ctx, nil); err != nil {
		log.Fatalf("Failed to wait for writer Sandbox: %v", err)
	}
	if _, err := writer.Terminate(context.Background(), nil); err != nil {
		log.Fatalf("Failed to terminate writer Sandbox: %v", err)
	}

	// List the directory contents from the client.
	entries, err := volume.ListDir(ctx, "/data", &modal.VolumeListDirParams{Recursive: true})
	if err != nil {
		log.Fatalf("Failed to list Volume directory: %v", err)
	}
	fmt.Println("Volume contents under /data:")
	for _, e := range entries {
		fmt.Printf("  %s (type=%d, size=%d)\n", e.Path, e.Type, e.Size)
	}

	// Read a file's contents directly from the client.
	data, err := volume.ReadFile(ctx, "/data/message.txt", nil)
	if err != nil {
		log.Fatalf("Failed to read Volume file: %v", err)
	}
	fmt.Printf("message.txt: %s", string(data))

	// Copy a file within the Volume.
	if err := volume.CopyFiles(ctx, []string{"/data/message.txt"}, "/data/message-copy.txt", nil); err != nil {
		log.Fatalf("Failed to copy Volume file: %v", err)
	}
	fmt.Println("Copied message.txt -> message-copy.txt")

	// Remove a file from the Volume.
	if err := volume.RemoveFile(ctx, "/data/other.txt", nil); err != nil {
		log.Fatalf("Failed to remove Volume file: %v", err)
	}
	fmt.Println("Removed other.txt")

	// Note: Volume.Commit and Volume.Reload operate on a Volume mounted inside a
	// running container (e.g. from a Sandbox after writing files), not from the
	// client control plane, so they are not demonstrated here.

	fmt.Println("Done.")
}
