// Package gdrive implements media.Volume backed by Google Drive. Drive is not an object
// store and has no gocloud.dev/blob driver, so it cannot fold into the cloud medium; it is
// its own type over the Drive REST API (google.golang.org/api/drive/v3).
//
// Like disk and cloud, Drive is ADDRESS-IDENTIFIED: a folder + path names a file
// unambiguously, so it carries no on-medium label and runs none of the label-verify /
// changer / spanning machinery. The run layout — clean payloads plus JSON header sidecars
// under runs/<run>/ — lives in package fslike, shared with disk and cloud, so a run streams
// between them byte-for-byte; this package supplies only the Drive storage primitives (a
// driveStore over the narrow driveAPI seam) plus auth and the `nb login` OAuth bootstrap.
//
// Credentials come from the ambient environment (GOOGLE_APPLICATION_CREDENTIALS), never the
// config — a service-account key for unattended Workspace Shared-Drive backups, or an OAuth
// authorized-user token (`nb login <medium>`) for a personal Google Drive. The scope is
// drive.file, so the app only ever touches files it created.
package gdrive

import (
	"context"
	"fmt"
	"strings"

	"github.com/Niloen/nbackup/internal/media"
	"github.com/Niloen/nbackup/internal/media/fslike"
)

func init() {
	// Drive is fslike-backed like disk and cloud, so it inherits the size profile and the
	// concurrent-write capability from the shared layout. Drive-specific: its folder/prefix
	// params, a part-size policy (it splits a large archive into part-files), the login
	// bootstrap, and pricing.
	s := fslike.Spec()
	s.Type = "gdrive"
	s.Params = []string{"folder", "prefix", "part_size"}
	// Split each archive into <= part_size part-files (default 10 GiB). Drive has no S3-style
	// 10000-part multipart ceiling, so the cap is only for manageability + resumability: a
	// large archive lands as several ordered part-files rather than one multi-hundred-GB
	// upload with no mid-file resume boundary. Splitting stays fully concurrent (unlike a
	// serial tape), and the layout matches cloud so disk<->cloud<->gdrive copies are identical.
	s.PartSize = media.PartSizePolicy{
		Default: 10 << 30,  // 10 GiB
		Max:     100 << 30, // 100 GiB
		MaxNote: "Google Drive uploads a large file as one resumable stream; keep part_size modest so an interrupted upload re-sends less",
	}
	s.Cost = newCost
	s.Login = login
	s.New = func(opts media.Options) (media.Volume, error) {
		folder := opts.Get("folder")
		if folder == "" {
			return nil, fmt.Errorf("gdrive medium requires a folder (a Drive folder ID or Shared-Drive ID to store backups under)")
		}
		ctx := context.Background()
		svc, err := openService(ctx)
		if err != nil {
			return nil, err
		}
		api := &realAPI{ctx: ctx, svc: svc}
		rootID := folder
		// An optional prefix is a subfolder path under the configured folder — the peer of
		// the cloud medium's key prefix, letting several catalogs share one Drive folder.
		if prefix := strings.Trim(opts.Get("prefix"), "/"); prefix != "" {
			seed := newStore(api, folder)
			seed.mu.Lock()
			id, _, err := seed.dirIDLocked(prefix, true)
			seed.mu.Unlock()
			if err != nil {
				return nil, fmt.Errorf("gdrive prefix %q: %w", prefix, err)
			}
			rootID = id
		}
		return fslike.Open(newStore(api, rootID))
	}
	media.Register(s)
}
