package gdrive

import (
	"context"
	"fmt"
	"io"

	"google.golang.org/api/drive/v3"
	"google.golang.org/api/googleapi"
)

// folderMime is the Drive mime type that marks a file as a folder. Backups live under
// a folder tree (root / runs / <run> / <files>), so every path segment but the leaf is
// a file of this type.
const folderMime = "application/vnd.google-apps.folder"

// driveItem is one child of a folder as reported by driveAPI.list: its opaque Drive
// file id, its name, and whether it is itself a folder.
type driveItem struct {
	id     string
	name   string
	folder bool
}

// driveAPI is the narrow slice of the Google Drive API the store needs, factored out so
// the store can be tested against an in-memory fake (there is no gocloud driver and no
// official in-memory Drive, unlike the cloud medium's mem:// bucket). The real
// implementation (realAPI) is a thin adapter over *drive.Service; the fake lives in the
// test files. All addressing is by opaque file id — Drive has no paths and a folder may
// hold same-named children, so the store maps its run-layout paths to ids above this seam.
type driveAPI interface {
	// list returns the non-trashed children of the folder with id parentID.
	list(parentID string) ([]driveItem, error)
	// mkdir creates a folder named name under parentID and returns its id.
	mkdir(parentID, name string) (string, error)
	// upload streams r as a new file named name under parentID and returns its id.
	// It reads r to EOF; a canceled ctx aborts the upload (no committed file).
	upload(ctx context.Context, parentID, name string, r io.Reader) (string, error)
	// openRange opens a byte range of the file with id: [off, off+length) when length > 0,
	// [off, end) when length <= 0 and off > 0, and the whole file when off == 0 && length <= 0.
	openRange(id string, off, length int64) (io.ReadCloser, error)
	// remove deletes the file (or folder, recursively) with id.
	remove(id string) error
}

// realAPI adapts *drive.Service to driveAPI. ctx is stored (accepted debt, like the cloud
// medium's blobStore) because fslike.Store's read path carries no context.
type realAPI struct {
	ctx context.Context
	svc *drive.Service
}

// allDrives makes every call span both My Drive and shared drives, so one gdrive medium
// works against a personal folder (OAuth user token) or a Shared Drive (service account)
// without a config switch.
func (a *realAPI) list(parentID string) ([]driveItem, error) {
	var out []driveItem
	q := fmt.Sprintf("%q in parents and trashed = false", parentID)
	call := a.svc.Files.List().
		Q(q).
		Spaces("drive").
		Fields("nextPageToken, files(id, name, mimeType)").
		SupportsAllDrives(true).
		IncludeItemsFromAllDrives(true).
		PageSize(1000).
		Context(a.ctx)
	for {
		res, err := call.Do()
		if err != nil {
			return nil, err
		}
		for _, f := range res.Files {
			out = append(out, driveItem{id: f.Id, name: f.Name, folder: f.MimeType == folderMime})
		}
		if res.NextPageToken == "" {
			return out, nil
		}
		call = call.PageToken(res.NextPageToken)
	}
}

func (a *realAPI) mkdir(parentID, name string) (string, error) {
	f, err := a.svc.Files.Create(&drive.File{
		Name:     name,
		MimeType: folderMime,
		Parents:  []string{parentID},
	}).SupportsAllDrives(true).Fields("id").Context(a.ctx).Do()
	if err != nil {
		return "", err
	}
	return f.Id, nil
}

func (a *realAPI) upload(ctx context.Context, parentID, name string, r io.Reader) (string, error) {
	f, err := a.svc.Files.Create(&drive.File{
		Name:    name,
		Parents: []string{parentID},
	}).Media(r).SupportsAllDrives(true).Fields("id").Context(ctx).Do()
	if err != nil {
		return "", err
	}
	return f.Id, nil
}

func (a *realAPI) openRange(id string, off, length int64) (io.ReadCloser, error) {
	call := a.svc.Files.Get(id).SupportsAllDrives(true).Context(a.ctx)
	// A sub-range is Drive's ranged media download (the HTTP Range header) — the whole
	// point of the framed shape on Drive: a selective restore pays for the covering
	// frames' bytes, not the object's. A whole read sets no header.
	if off > 0 || length > 0 {
		if length > 0 {
			call.Header().Set("Range", fmt.Sprintf("bytes=%d-%d", off, off+length-1))
		} else {
			call.Header().Set("Range", fmt.Sprintf("bytes=%d-", off))
		}
	}
	resp, err := call.Download()
	if err != nil {
		return nil, err
	}
	return resp.Body, nil
}

func (a *realAPI) remove(id string) error {
	err := a.svc.Files.Delete(id).SupportsAllDrives(true).Context(a.ctx).Do()
	// A file already gone is not an error (idempotent reclamation), matching the
	// cloud medium's NotFound tolerance.
	if gErr, ok := err.(*googleapi.Error); ok && gErr.Code == 404 {
		return nil
	}
	return err
}
