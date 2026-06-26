package modal

import (
	"bytes"
	"context"
	"crypto/sha256"
	"fmt"
	"io"
	"iter"
	"net/http"
	"os"
	"path"
	"path/filepath"
	"strings"

	pb "github.com/modal-labs/modal-client/go/proto/modal_proto"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
)

// VolumeService provides Volume related operations.
type VolumeService interface {
	FromName(ctx context.Context, name string, params *VolumeFromNameParams) (*Volume, error)
	FromID(ctx context.Context, volumeID string, params *VolumeFromIDParams) (*Volume, error)
	Ephemeral(ctx context.Context, params *VolumeEphemeralParams) (*Volume, error)
	Delete(ctx context.Context, name string, params *VolumeDeleteParams) error
	List(ctx context.Context, params *VolumeListParams) ([]*VolumeListItem, error)
}

type volumeServiceImpl struct{ client *Client }

// Volume represents a Modal Volume that provides persistent storage.
type Volume struct {
	VolumeID        string
	Name            string
	mountOptions    resolvedVolumeMountOptions
	cancelEphemeral context.CancelFunc

	client *Client
}

// resolvedVolumeMountOptions is the resolved mount configuration stored on a Volume.
// The zero value represents an unconfigured (default) mount.
type resolvedVolumeMountOptions struct {
	readOnly bool
	subPath  string
}

// VolumeMountOptions are options for mounting a Volume. Fields are pointers so unset values
// preserve the corresponding option from a previous WithMountOptions call (stacking).
type VolumeMountOptions struct {
	ReadOnly *bool
	SubPath  *string
}

// volumeToMountProto builds the VolumeMount proto from a Volume's mount configuration.
func volumeToMountProto(mountPath string, volume *Volume) *pb.VolumeMount {
	var subPath *string
	if volume.mountOptions.subPath != "" {
		subPath = &volume.mountOptions.subPath
	}

	return pb.VolumeMount_builder{
		VolumeId:               volume.VolumeID,
		MountPath:              mountPath,
		AllowBackgroundCommits: true,
		ReadOnly:               volume.mountOptions.readOnly,
		SubPath:                subPath,
	}.Build()
}

// VolumeFsVersion selects the Modal Volume filesystem format used when CREATING a
// volume.
type VolumeFsVersion = pb.VolumeFsVersion

const (
	// VolumeVersionUnspecified lets the SDK choose the default format, which is v2.
	VolumeVersionUnspecified = pb.VolumeFsVersion_VOLUME_FS_VERSION_UNSPECIFIED
	// VolumeVersionV1 is the original Volume format.
	VolumeVersionV1 = pb.VolumeFsVersion_VOLUME_FS_VERSION_V1
	// VolumeVersionV2 is the newer Volume format and the SDK default. It is
	// required by the Volume file-management APIs (ListDir, ReadFile, etc.).
	VolumeVersionV2 = pb.VolumeFsVersion_VOLUME_FS_VERSION_V2
)

// defaultVolumeFsVersion is the format the SDK uses when creating a Volume
// without an explicit Version.
const defaultVolumeFsVersion = VolumeVersionV2

// resolveVolumeVersion returns version, substituting the SDK default (v2) when
// version is left unspecified.
func resolveVolumeVersion(version VolumeFsVersion) VolumeFsVersion {
	if version == VolumeVersionUnspecified {
		return defaultVolumeFsVersion
	}
	return version
}

// VolumeFromNameParams are options for finding Modal Volumes.
type VolumeFromNameParams struct {
	Environment     string
	CreateIfMissing bool
	// Version selects the filesystem format when CreateIfMissing creates a new
	// volume. Ignored for an already-existing volume (it's returned as-is). The
	// zero value (VolumeVersionUnspecified) creates a v2 Volume — the SDK default.
	Version VolumeFsVersion
}

// FromName references a Volume by its name.
func (s *volumeServiceImpl) FromName(ctx context.Context, name string, params *VolumeFromNameParams) (*Volume, error) {
	if params == nil {
		params = &VolumeFromNameParams{}
	}

	creationType := pb.ObjectCreationType_OBJECT_CREATION_TYPE_UNSPECIFIED
	if params.CreateIfMissing {
		creationType = pb.ObjectCreationType_OBJECT_CREATION_TYPE_CREATE_IF_MISSING
	}

	resp, err := s.client.cpClient.VolumeGetOrCreate(ctx, pb.VolumeGetOrCreateRequest_builder{
		DeploymentName:     name,
		EnvironmentName:    firstNonEmpty(params.Environment, s.client.profile.Environment),
		ObjectCreationType: creationType,
		Version:            resolveVolumeVersion(params.Version),
	}.Build())

	if status, ok := status.FromError(err); ok && status.Code() == codes.NotFound {
		return nil, NotFoundError{fmt.Sprintf("Volume '%s' not found", name)}
	}
	if err != nil {
		return nil, err
	}

	s.client.logger.DebugContext(ctx, "Retrieved Volume", "volume_id", resp.GetVolumeId(), "volume_name", name)
	return &Volume{VolumeID: resp.GetVolumeId(), Name: name, cancelEphemeral: nil, client: s.client}, nil
}

// VolumeFromIDParams are options for client.Volumes.FromID.
type VolumeFromIDParams struct{}

// FromID references an existing Volume by its ID (e.g. "vo-...").
func (s *volumeServiceImpl) FromID(ctx context.Context, volumeID string, params *VolumeFromIDParams) (*Volume, error) {
	resp, err := s.client.cpClient.VolumeGetById(ctx, pb.VolumeGetByIdRequest_builder{
		VolumeId: volumeID,
	}.Build())
	if st, ok := status.FromError(err); ok && st.Code() == codes.NotFound {
		return nil, NotFoundError{fmt.Sprintf("Volume '%s' not found", volumeID)}
	}
	if err != nil {
		return nil, err
	}

	name := ""
	if m := resp.GetMetadata(); m != nil {
		name = m.GetName()
	}
	return &Volume{VolumeID: resp.GetVolumeId(), Name: name, cancelEphemeral: nil, client: s.client}, nil
}

// WithMountOptions configures how a Volume is mounted. Fields left as nil on options preserve
// the corresponding value from any previous WithMountOptions call on the same Volume (stacking).
func (v *Volume) WithMountOptions(options *VolumeMountOptions) *Volume {
	merged := v.mountOptions
	if options != nil {
		if options.ReadOnly != nil {
			merged.readOnly = *options.ReadOnly
		}
		if options.SubPath != nil {
			if *options.SubPath == "/" {
				merged.subPath = ""
			} else {
				merged.subPath = *options.SubPath
			}
		}
	}
	return &Volume{
		VolumeID:        v.VolumeID,
		Name:            v.Name,
		mountOptions:    merged,
		cancelEphemeral: v.cancelEphemeral,
		client:          v.client,
	}
}

// VolumeEphemeralParams are options for client.Volumes.Ephemeral.
type VolumeEphemeralParams struct {
	Environment string
}

// VolumeDeleteParams are options for client.Volumes.Delete.
type VolumeDeleteParams struct {
	Environment  string
	AllowMissing bool
}

// Ephemeral creates a nameless, temporary Volume, that persists until CloseEphemeral is called, or the process exits.
func (s *volumeServiceImpl) Ephemeral(ctx context.Context, params *VolumeEphemeralParams) (*Volume, error) {
	if params == nil {
		params = &VolumeEphemeralParams{}
	}

	resp, err := s.client.cpClient.VolumeGetOrCreate(ctx, pb.VolumeGetOrCreateRequest_builder{
		ObjectCreationType: pb.ObjectCreationType_OBJECT_CREATION_TYPE_EPHEMERAL,
		EnvironmentName:    firstNonEmpty(params.Environment, s.client.profile.Environment),
		Version:            resolveVolumeVersion(VolumeVersionUnspecified),
	}.Build())
	if err != nil {
		return nil, err
	}

	s.client.logger.DebugContext(ctx, "Created ephemeral Volume", "volume_id", resp.GetVolumeId())

	ephemeralCtx, cancel := context.WithCancel(context.Background())
	startEphemeralHeartbeat(ephemeralCtx, func() error {
		_, err := s.client.cpClient.VolumeHeartbeat(ephemeralCtx, pb.VolumeHeartbeatRequest_builder{
			VolumeId: resp.GetVolumeId(),
		}.Build())
		return err
	})

	return &Volume{
		VolumeID:        resp.GetVolumeId(),
		cancelEphemeral: cancel,
		client:          s.client,
	}, nil
}

// CloseEphemeral deletes an ephemeral Volume, only used with VolumeEphemeral.
func (v *Volume) CloseEphemeral() {
	if v.cancelEphemeral != nil {
		v.cancelEphemeral()
	} else {
		// We panic in this case because of invalid usage. In general, methods
		// used with `defer` like CloseEphemeral should not return errors.
		panic(fmt.Sprintf("Volume %s is not ephemeral", v.VolumeID))
	}
}

// Delete deletes a named Volume.
//
// Warning: Deletion is irreversible and will affect any Apps currently using the Volume.
func (s *volumeServiceImpl) Delete(ctx context.Context, name string, params *VolumeDeleteParams) error {
	if params == nil {
		params = &VolumeDeleteParams{}
	}

	volume, err := s.FromName(ctx, name, &VolumeFromNameParams{
		Environment:     params.Environment,
		CreateIfMissing: false,
	})

	if err != nil {
		if _, ok := err.(NotFoundError); ok && params.AllowMissing {
			return nil
		}
		return err
	}

	_, err = s.client.cpClient.VolumeDelete(ctx, pb.VolumeDeleteRequest_builder{
		VolumeId: volume.VolumeID,
	}.Build())

	if err != nil {
		if st, ok := status.FromError(err); ok && st.Code() == codes.NotFound && params.AllowMissing {
			return nil
		}
		return err
	}

	s.client.logger.DebugContext(ctx, "Deleted Volume", "volume_name", name, "volume_id", volume.VolumeID)
	return nil
}

// VolumeListItem holds summary information about a named Volume.
type VolumeListItem struct {
	VolumeID  string
	Name      string
	CreatedAt float64
}

// FileEntryType mirrors the proto FileEntry.FileType enum.
type FileEntryType int32

const (
	FileEntryTypeUnspecified FileEntryType = 0
	FileEntryTypeFile        FileEntryType = 1
	FileEntryTypeDirectory   FileEntryType = 2
	FileEntryTypeSymlink     FileEntryType = 3
	FileEntryTypeFIFO        FileEntryType = 4
	FileEntryTypeSocket      FileEntryType = 5
)

// FileEntry describes a file or directory inside a Volume.
type FileEntry struct {
	Path  string
	Type  FileEntryType
	Mtime uint64
	Size  uint64
}

// VolumeListParams are options for client.Volumes.List.
type VolumeListParams struct {
	Environment string
}

// VolumeCommitParams are options for Volume.Commit.
type VolumeCommitParams struct{}

// VolumeReloadParams are options for Volume.Reload.
type VolumeReloadParams struct{}

// VolumeRenameParams are options for Volume.Rename.
type VolumeRenameParams struct{}

// VolumeListDirParams are options for Volume.ListDir and Volume.IterDir.
type VolumeListDirParams struct {
	Recursive bool
}

// VolumeReadFileParams are options for Volume.ReadFile and Volume.ReadFileStream.
type VolumeReadFileParams struct {
	// Start is the byte offset to start reading from (default 0).
	Start uint64
	// Len is the number of bytes to read; 0 means read to the end of the file.
	Len uint64
}

// VolumeCopyFilesParams are options for Volume.CopyFiles.
type VolumeCopyFilesParams struct {
	Recursive bool
}

// VolumeRemoveFileParams are options for Volume.RemoveFile.
type VolumeRemoveFileParams struct {
	Recursive bool
}

// VolumePutFileParams are options for Volume.PutFile and Volume.PutFileFromLocal.
type VolumePutFileParams struct {
	// Mode sets the Unix file permission bits on the uploaded file. Defaults to
	// 0644 (or the local file's mode for PutFileFromLocal) when nil.
	Mode *uint32
	// Force allows overwriting an existing file. When false (the default), the
	// upload fails with AlreadyExistsError if the destination path already exists.
	Force bool
}

// List returns all named Volumes in the given environment.
func (s *volumeServiceImpl) List(ctx context.Context, params *VolumeListParams) ([]*VolumeListItem, error) {
	if params == nil {
		params = &VolumeListParams{}
	}

	resp, err := s.client.cpClient.VolumeList(ctx, pb.VolumeListRequest_builder{
		EnvironmentName: firstNonEmpty(params.Environment, s.client.profile.Environment),
	}.Build())
	if err != nil {
		return nil, err
	}

	items := make([]*VolumeListItem, 0, len(resp.GetItems()))
	for _, item := range resp.GetItems() {
		name := item.GetLabel()
		if m := item.GetMetadata(); m != nil && m.GetName() != "" {
			name = m.GetName()
		}
		items = append(items, &VolumeListItem{
			VolumeID:  item.GetVolumeId(),
			Name:      name,
			CreatedAt: item.GetCreatedAt(),
		})
	}
	return items, nil
}

// Rename renames a Volume.
func (v *Volume) Rename(ctx context.Context, newName string, params *VolumeRenameParams) error {
	_, err := v.client.cpClient.VolumeRename(ctx, pb.VolumeRenameRequest_builder{
		VolumeId: v.VolumeID,
		Name:     newName,
	}.Build())
	if err != nil {
		return err
	}
	v.Name = newName
	return nil
}

// Commit persists any changes made to a mounted Volume so they are visible to other containers.
func (v *Volume) Commit(ctx context.Context, params *VolumeCommitParams) error {
	_, err := v.client.cpClient.VolumeCommit(ctx, pb.VolumeCommitRequest_builder{
		VolumeId: v.VolumeID,
	}.Build())
	return err
}

// Reload makes the latest committed state of the Volume available in the running container.
// Reloading will fail if there are open files for the Volume.
func (v *Volume) Reload(ctx context.Context, params *VolumeReloadParams) error {
	_, err := v.client.cpClient.VolumeReload(ctx, pb.VolumeReloadRequest_builder{
		VolumeId: v.VolumeID,
	}.Build())
	return err
}

// VolumeInfo holds metadata about a Volume.
type VolumeInfo struct {
	VolumeID  string
	Name      string
	Version   VolumeFsVersion
	CreatedAt float64
	CreatedBy string
}

// VolumeInfoParams are options for Volume.Info.
type VolumeInfoParams struct{}

// Info returns metadata about the Volume.
func (v *Volume) Info(ctx context.Context, params *VolumeInfoParams) (*VolumeInfo, error) {
	resp, err := v.client.cpClient.VolumeGetById(ctx, pb.VolumeGetByIdRequest_builder{
		VolumeId: v.VolumeID,
	}.Build())
	if err != nil {
		return nil, err
	}

	info := &VolumeInfo{VolumeID: resp.GetVolumeId()}
	if m := resp.GetMetadata(); m != nil {
		info.Name = m.GetName()
		info.Version = m.GetVersion()
		if ci := m.GetCreationInfo(); ci != nil {
			info.CreatedAt = ci.GetCreatedAt()
			info.CreatedBy = ci.GetCreatedBy()
		}
	}
	return info, nil
}

// ListDir lists files and directories under path. Use params.Recursive to recurse.
func (v *Volume) ListDir(ctx context.Context, path string, params *VolumeListDirParams) ([]FileEntry, error) {
	var entries []FileEntry
	for entry, err := range v.IterDir(ctx, path, params) {
		if err != nil {
			return nil, err
		}
		entries = append(entries, entry)
	}
	return entries, nil
}

// IterDir returns an iterator over files and directories under path.
func (v *Volume) IterDir(ctx context.Context, path string, params *VolumeListDirParams) iter.Seq2[FileEntry, error] {
	if params == nil {
		params = &VolumeListDirParams{}
	}
	return func(yield func(FileEntry, error) bool) {
		stream, err := v.client.cpClient.VolumeListFiles2(ctx, pb.VolumeListFiles2Request_builder{
			VolumeId:  v.VolumeID,
			Path:      path,
			Recursive: params.Recursive,
		}.Build())
		if err != nil {
			yield(FileEntry{}, err)
			return
		}
		for {
			resp, err := stream.Recv()
			if err == io.EOF {
				return
			}
			if err != nil {
				yield(FileEntry{}, err)
				return
			}
			for _, e := range resp.GetEntries() {
				if !yield(fileEntryFromProto(e), nil) {
					return
				}
			}
		}
	}
}

// ReadFile reads a file from the Volume and returns its contents.
// For large files prefer ReadFileStream to avoid loading everything into memory.
func (v *Volume) ReadFile(ctx context.Context, path string, params *VolumeReadFileParams) ([]byte, error) {
	var buf bytes.Buffer
	for chunk, err := range v.ReadFileStream(ctx, path, params) {
		if err != nil {
			return nil, err
		}
		buf.Write(chunk)
	}
	return buf.Bytes(), nil
}

// ReadFileStream returns an iterator that yields successive byte chunks of a Volume file.
func (v *Volume) ReadFileStream(ctx context.Context, path string, params *VolumeReadFileParams) iter.Seq2[[]byte, error] {
	if params == nil {
		params = &VolumeReadFileParams{}
	}
	return func(yield func([]byte, error) bool) {
		resp, err := v.client.cpClient.VolumeGetFile2(ctx, pb.VolumeGetFile2Request_builder{
			VolumeId: v.VolumeID,
			Path:     path,
			Start:    params.Start,
			Len:      params.Len,
		}.Build())
		if err != nil {
			yield(nil, err)
			return
		}

		for _, url := range resp.GetGetUrls() {
			data, err := fetchURL(ctx, url)
			if err != nil {
				yield(nil, err)
				return
			}
			if !yield(data, nil) {
				return
			}
		}
	}
}

// ReadFileInto streams a Volume file's contents into w. It avoids buffering the
// whole file in memory, so it suits large files. Returns the number of bytes written.
func (v *Volume) ReadFileInto(ctx context.Context, path string, w io.Writer, params *VolumeReadFileParams) (int64, error) {
	var written int64
	for chunk, err := range v.ReadFileStream(ctx, path, params) {
		if err != nil {
			return written, err
		}
		n, werr := w.Write(chunk)
		written += int64(n)
		if werr != nil {
			return written, werr
		}
	}
	return written, nil
}

// RemoveFile removes a file or directory from the Volume.
func (v *Volume) RemoveFile(ctx context.Context, path string, params *VolumeRemoveFileParams) error {
	if params == nil {
		params = &VolumeRemoveFileParams{}
	}
	_, err := v.client.cpClient.VolumeRemoveFile2(ctx, pb.VolumeRemoveFile2Request_builder{
		VolumeId:  v.VolumeID,
		Path:      path,
		Recursive: params.Recursive,
	}.Build())
	return err
}

// CopyFiles copies files within the Volume from srcPaths to dstPath.
// Semantics follow UNIX cp.
func (v *Volume) CopyFiles(ctx context.Context, srcPaths []string, dstPath string, params *VolumeCopyFilesParams) error {
	if params == nil {
		params = &VolumeCopyFilesParams{}
	}
	_, err := v.client.cpClient.VolumeCopyFiles2(ctx, pb.VolumeCopyFiles2Request_builder{
		VolumeId:  v.VolumeID,
		SrcPaths:  srcPaths,
		DstPath:   dstPath,
		Recursive: params.Recursive,
	}.Build())
	return err
}

// fileEntryFromProto converts a proto FileEntry to the local FileEntry type.
func fileEntryFromProto(e *pb.FileEntry) FileEntry {
	return FileEntry{
		Path:  e.GetPath(),
		Type:  FileEntryType(e.GetType()),
		Mtime: e.GetMtime(),
		Size:  e.GetSize(),
	}
}

// fetchURL performs an HTTP GET and returns the response body. Used to download
// Volume file contents from the presigned URLs returned by VolumeGetFile2.
func fetchURL(ctx context.Context, url string) ([]byte, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return nil, err
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("unexpected HTTP status %d fetching Volume file", resp.StatusCode)
	}
	return io.ReadAll(resp.Body)
}

// volumeBlockSize is the block size (8 MiB) used by the v2 Volume upload
// protocol; it must match the server's block size.
const volumeBlockSize = 8 * 1024 * 1024

// volumeUploadBlock describes one 8 MiB block of a file being uploaded. The
// byte range [start,end) is the block content with trailing zeros trimmed (a
// sparse-file optimization matching the Python client); the omitted zeros are
// reconstructed from the file size on the server.
type volumeUploadBlock struct {
	start int64
	end   int64
	hash  [sha256.Size]byte
}

// volumeUploadFile is a single file staged for upload via VolumePutFiles2. Its
// content is read lazily by byte range from open() — both when hashing blocks
// and when uploading them — so a file is never fully buffered in memory (the
// data source is re-opened per operation, matching the Python client).
type volumeUploadFile struct {
	path   string
	mode   uint32
	size   int64
	open   func() (io.ReadSeekCloser, error)
	blocks []volumeUploadBlock
}

// nopReadSeekCloser adds a no-op Close to an in-memory io.ReadSeeker.
type nopReadSeekCloser struct{ io.ReadSeeker }

func (nopReadSeekCloser) Close() error { return nil }

// PutFile writes data to remotePath in the Volume directly, without mounting it
// in a Sandbox. Requires a v2 Volume.
//
// By default the upload fails with AlreadyExistsError if remotePath already
// exists; set params.Force to overwrite.
func (v *Volume) PutFile(ctx context.Context, remotePath string, data []byte, params *VolumePutFileParams) error {
	mode := uint32(0o644)
	force := false
	if params != nil {
		if params.Mode != nil {
			mode = *params.Mode
		}
		force = params.Force
	}
	file, err := volumeUploadFileFromBytes(remotePath, data, mode)
	if err != nil {
		return err
	}
	return v.putFiles(ctx, []volumeUploadFile{file}, force)
}

// PutFileFromLocal uploads the contents of the local file at localPath to
// remotePath in the Volume directly, without mounting it in a Sandbox. The file
// is streamed from disk block by block rather than buffered in memory.
// Requires a v2 Volume.
func (v *Volume) PutFileFromLocal(ctx context.Context, localPath, remotePath string, params *VolumePutFileParams) error {
	var modeOverride *uint32
	force := false
	if params != nil {
		modeOverride = params.Mode
		force = params.Force
	}
	file, err := volumeUploadFileFromPath(remotePath, localPath, modeOverride)
	if err != nil {
		return err
	}
	return v.putFiles(ctx, []volumeUploadFile{file}, force)
}

// VolumePutDirectoryParams are options for Volume.PutDirectory.
type VolumePutDirectoryParams struct {
	// Recursive includes files in nested subdirectories. Defaults to true when nil.
	Recursive *bool
	// Force allows overwriting existing files. When false (the default), the
	// upload fails with AlreadyExistsError if a destination path already exists.
	Force bool
}

// PutDirectory uploads every regular file under the local directory localDir to
// remoteDir in the Volume directly, without mounting it in a Sandbox. All files
// are uploaded in a single batch. By default it recurses into subdirectories.
// Requires a v2 Volume.
func (v *Volume) PutDirectory(ctx context.Context, localDir, remoteDir string, params *VolumePutDirectoryParams) error {
	recursive := true
	force := false
	if params != nil {
		if params.Recursive != nil {
			recursive = *params.Recursive
		}
		force = params.Force
	}

	var files []volumeUploadFile
	walkErr := filepath.WalkDir(localDir, func(p string, d os.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.IsDir() {
			if !recursive && p != localDir {
				return filepath.SkipDir
			}
			return nil
		}
		if !d.Type().IsRegular() {
			return nil // skip symlinks, sockets, devices, etc.
		}
		rel, err := filepath.Rel(localDir, p)
		if err != nil {
			return err
		}
		file, err := volumeUploadFileFromPath(path.Join(remoteDir, filepath.ToSlash(rel)), p, nil)
		if err != nil {
			return err
		}
		files = append(files, file)
		return nil
	})
	if walkErr != nil {
		return walkErr
	}
	if len(files) == 0 {
		return nil
	}
	return v.putFiles(ctx, files, force)
}

// volumeUploadFileFromBytes stages in-memory data for upload.
func volumeUploadFileFromBytes(remotePath string, data []byte, mode uint32) (volumeUploadFile, error) {
	open := func() (io.ReadSeekCloser, error) { return nopReadSeekCloser{bytes.NewReader(data)}, nil }
	blocks, err := hashVolumeBlocks(open, int64(len(data)))
	if err != nil {
		return volumeUploadFile{}, err
	}
	return volumeUploadFile{path: remotePath, mode: mode, size: int64(len(data)), open: open, blocks: blocks}, nil
}

// volumeUploadFileFromPath stages a local file for upload, streaming it from
// disk. When modeOverride is nil the local file's permission bits are used.
func volumeUploadFileFromPath(remotePath, localPath string, modeOverride *uint32) (volumeUploadFile, error) {
	fi, err := os.Stat(localPath)
	if err != nil {
		return volumeUploadFile{}, err
	}
	mode := uint32(fi.Mode().Perm())
	if modeOverride != nil {
		mode = *modeOverride
	}
	open := func() (io.ReadSeekCloser, error) { return os.Open(localPath) }
	blocks, err := hashVolumeBlocks(open, fi.Size())
	if err != nil {
		return volumeUploadFile{}, err
	}
	return volumeUploadFile{path: remotePath, mode: mode, size: fi.Size(), open: open, blocks: blocks}, nil
}

// hashVolumeBlocks reads the source one 8 MiB block at a time and hashes each
// (trailing zeros trimmed) for the content-addressed VolumePutFiles2 protocol.
// At most one block is held in memory at a time.
func hashVolumeBlocks(open func() (io.ReadSeekCloser, error), size int64) ([]volumeUploadBlock, error) {
	rc, err := open()
	if err != nil {
		return nil, err
	}
	defer rc.Close()

	var blocks []volumeUploadBlock
	buf := make([]byte, volumeBlockSize)
	for start := int64(0); start < size; start += volumeBlockSize {
		end := start + volumeBlockSize
		if end > size {
			end = size
		}
		n := int(end - start)
		if _, err := rc.Seek(start, io.SeekStart); err != nil {
			return nil, err
		}
		if _, err := io.ReadFull(rc, buf[:n]); err != nil {
			return nil, err
		}
		e := n
		for e > 0 && buf[e-1] == 0 {
			e--
		}
		blocks = append(blocks, volumeUploadBlock{start: start, end: start + int64(e), hash: sha256.Sum256(buf[:e])})
	}
	return blocks, nil
}

// putFiles runs the two-pass VolumePutFiles2 protocol: the first call surfaces
// any blocks the server is missing, those blocks are streamed to the returned
// presigned URLs, and the second call (carrying each upload's response token)
// completes the put.
func (v *Volume) putFiles(ctx context.Context, files []volumeUploadFile, force bool) error {
	putResponses := map[[sha256.Size]byte][]byte{}

	for pass := 0; pass < 2; pass++ {
		reqFiles := make([]*pb.VolumePutFiles2Request_File, len(files))
		for i, f := range files {
			blocks := make([]*pb.VolumePutFiles2Request_Block, len(f.blocks))
			for j, b := range f.blocks {
				hash := b.hash
				blocks[j] = pb.VolumePutFiles2Request_Block_builder{
					ContentsSha256: hash[:],
					PutResponse:    putResponses[b.hash],
				}.Build()
			}
			mode := f.mode
			reqFiles[i] = pb.VolumePutFiles2Request_File_builder{
				Path:   f.path,
				Size:   uint64(f.size),
				Mode:   &mode,
				Blocks: blocks,
			}.Build()
		}

		resp, err := v.client.cpClient.VolumePutFiles2(ctx, pb.VolumePutFiles2Request_builder{
			VolumeId:                       v.VolumeID,
			Files:                          reqFiles,
			DisallowOverwriteExistingFiles: !force,
		}.Build())
		if err != nil {
			// The server may signal an overwrite conflict via the AlreadyExists
			// code or an InvalidArgument carrying an "already exists" message.
			if st, ok := status.FromError(err); ok &&
				(st.Code() == codes.AlreadyExists || strings.Contains(st.Message(), "already exists")) {
				return AlreadyExistsError{st.Message()}
			}
			return err
		}

		missing := resp.GetMissingBlocks()
		if len(missing) == 0 {
			return nil
		}
		if pass == 1 {
			return fmt.Errorf("volume put failed: server still reports missing blocks after upload")
		}

		for _, mb := range missing {
			f := files[mb.GetFileIndex()]
			b := f.blocks[mb.GetBlockIndex()]
			respBody, err := uploadVolumeBlock(ctx, mb.GetPutUrl(), f, b)
			if err != nil {
				return err
			}
			putResponses[b.hash] = respBody
		}
	}
	return nil
}

// uploadVolumeBlock streams block b of file f to a presigned URL with an HTTP
// PUT and returns the response body, which VolumePutFiles2 requires as the
// block's put_response. The block is read from the source by range, not buffered.
func uploadVolumeBlock(ctx context.Context, url string, f volumeUploadFile, b volumeUploadBlock) ([]byte, error) {
	rc, err := f.open()
	if err != nil {
		return nil, err
	}
	defer rc.Close()
	if _, err := rc.Seek(b.start, io.SeekStart); err != nil {
		return nil, err
	}
	length := b.end - b.start

	req, err := http.NewRequestWithContext(ctx, http.MethodPut, url, io.LimitReader(rc, length))
	if err != nil {
		return nil, err
	}
	req.ContentLength = length
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	respBody, _ := io.ReadAll(resp.Body)
	if resp.StatusCode >= 400 {
		return nil, fmt.Errorf("Volume block upload failed with status %d: %s", resp.StatusCode, string(respBody))
	}
	return respBody, nil
}
