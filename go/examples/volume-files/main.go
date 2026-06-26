package main

import (
	"context"
	"fmt"
	"log"

	modal "github.com/modal-labs/modal-client/go"
)

// This example demonstrates reading and writing Volume files directly from the
// client — no Sandbox required. These APIs require a v2 Volume, which is the SDK
// default when Version is left unspecified.
func main() {
	ctx := context.Background()
	mc, err := modal.NewClient()
	if err != nil {
		log.Fatalf("Failed to create client: %v", err)
	}

	volume, err := mc.Volumes.FromName(ctx, "libmodal-volume-files-example", &modal.VolumeFromNameParams{
		CreateIfMissing: true,
	})
	if err != nil {
		log.Fatalf("Failed to create Volume: %v", err)
	}

	info, err := volume.Info(ctx, nil)
	if err != nil {
		log.Fatalf("Failed to get Volume info: %v", err)
	}
	fmt.Printf("Volume %q (version %v)\n", info.Name, info.Version)

	// Write files directly to the Volume (overwrite if they already exist).
	force := &modal.VolumePutFileParams{Force: true}
	if err := volume.PutFile(ctx, "/data/message.txt", []byte("hello from modal\n"), force); err != nil {
		log.Fatalf("Failed to write Volume file: %v", err)
	}
	if err := volume.PutFile(ctx, "/data/other.txt", []byte("second file\n"), force); err != nil {
		log.Fatalf("Failed to write Volume file: %v", err)
	}
	fmt.Println("Wrote /data/message.txt and /data/other.txt")

	// List the directory contents.
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

	fmt.Println("Done.")
}
