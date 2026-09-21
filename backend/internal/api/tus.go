package api

import (
	"context"
	"crypto/rand"
	"errors"
	"fmt"
	"net/http"
	"os"
	"strings"

	tushandler "github.com/tus/tusd/v2/pkg/handler"
	expslog "golang.org/x/exp/slog"

	"github.com/ngaitonde/EASBirdNet/backend/internal/db"
	"github.com/ngaitonde/EASBirdNet/backend/internal/storage"
)

// tusPath is where browsers send card audio, one file per upload, over the
// tus resumable upload protocol (https://tus.io): POST creates an upload, PATCH
// appends to it and HEAD asks how much has landed, so a dropped connection
// carries on from the last byte rather than the start of the file.
//
// tusd does the protocol. What Birdsense adds is who may upload what
// (tusAccess, beforeFileUpload) and what a finished file means for its card
// (afterFileUpload).
const tusPath = "/api/v1/tus/"

// Upload-Metadata keys. The browser sends reference and path; the server
// replaces the metadata when it creates the upload, so the finish hook only
// ever reads values the server wrote.
const (
	metaReference = "reference"
	metaPath      = "path"
	metaAudioFile = "audioFileId"
	metaUserID    = "userId"
)

type userKey struct{}

// tusEndpoint is the tus handler behind the session check.
func (h *handlers) tusEndpoint() http.Handler {
	composer := tushandler.NewStoreComposer()
	h.files.UseIn(composer)
	tus, err := tushandler.NewHandler(tushandler.Config{
		StoreComposer: composer,
		BasePath:      tusPath,
		// No file on a card's list is anywhere near this -- cardList holds the
		// list to the same bound -- so it is the backstop for a length that was
		// never on one: a POST claiming more is refused before a byte is taken,
		// and a PATCH stops reading a body at it.
		MaxSize: maxFileBytes,
		// Browsers only send files here: nothing downloads them, deletes them,
		// or stitches partial uploads together.
		DisableDownload:      true,
		DisableTermination:   true,
		DisableConcatenation: true,
		// The frontend is served from the same origin.
		Cors: &tushandler.CorsConfig{Disable: true},
		// Behind the Container Apps ingress a request arrives as plain http, and
		// the upload URL handed back has to be https like the page.
		RespectForwardedHeaders: true,
		// tusd logs every request and chunk at Info, and requestLogger already
		// logs each request, so it keeps only warnings and errors (a store that
		// failed, a body that stopped arriving). It predates log/slog and takes
		// golang.org/x/exp/slog, so it writes to stdout itself.
		Logger:                    expslog.New(expslog.NewTextHandler(os.Stdout, &expslog.HandlerOptions{Level: expslog.LevelWarn})),
		PreUploadCreateCallback:   h.beforeFileUpload,
		PreFinishResponseCallback: h.afterFileUpload,
	})
	if err != nil {
		// Only a bad Config gets here: a programming error, like a bad mux pattern.
		panic(fmt.Sprintf("api: configuring the tus handler: %v", err))
	}
	return h.tusAccess(composer.Core, http.StripPrefix(strings.TrimSuffix(tusPath, "/"), tus))
}

// tusAccess applies the card rule to the tus endpoint. Creating an upload is
// checked in beforeFileUpload, where the metadata names the card. Every other
// request names an upload, and the upload is only reachable by someone who can
// see the card it was created for.
func (h *handlers) tusAccess(uploads tushandler.DataStore, next http.Handler) http.HandlerFunc {
	return h.requireSession(func(w http.ResponseWriter, r *http.Request, me db.User) {
		ctx := r.Context()
		if id := strings.Trim(strings.TrimPrefix(r.URL.Path, tusPath), "/"); id != "" {
			// Anything that isn't an id the server minted is refused before the
			// store is asked for it.
			if !mintedUploadID(id) {
				h.problem(w, http.StatusNotFound, "no such upload")
				return
			}
			// An upload that can't be read is left to tusd, which answers 404.
			if up, err := uploads.GetUpload(ctx, id); err == nil {
				info, err := up.GetInfo(ctx)
				if err != nil {
					h.fail(w, r, err)
					return
				}
				card, err := h.store.GetUpload(ctx, info.MetaData[metaReference])
				switch {
				case errors.Is(err, db.ErrNotFound) || (err == nil && !canSee(me, card)):
					h.problem(w, http.StatusNotFound, "no such upload")
					return
				case err != nil:
					h.fail(w, r, err)
					return
				}
			}
		}
		next.ServeHTTP(w, r.WithContext(context.WithValue(ctx, userKey{}, me)))
	})
}

// mintedUploadID reports whether an id is the shape beforeFileUpload gives
// every upload it creates: "{card}/{random}", two path elements of letters,
// digits, "-", "_" and ".", neither of them dots alone.
//
// Gotcha: Go's ServeMux cleans the *escaped* path, so a percent-encoded ".."
// survives routing and arrives here as a real path element -- without this,
// HEAD /api/v1/tus/%2e%2e/secret reads, and PATCH writes, "<dir>/secret"
// beside the uploads prefix rather than inside it.
func mintedUploadID(id string) bool {
	prefix, token, ok := strings.Cut(id, "/")
	return ok && idElement(prefix) && idElement(token)
}

// idElement is one element of an upload id: what storagePrefix leaves, and
// never a name that means a directory.
func idElement(s string) bool {
	if s == "" || strings.Trim(s, ".") == "" {
		return false
	}
	for _, r := range s {
		switch {
		case r >= 'a' && r <= 'z', r >= 'A' && r <= 'Z', r >= '0' && r <= '9', r == '-', r == '_', r == '.':
		default:
			return false
		}
	}
	return true
}

// beforeFileUpload decides whether a file may be sent: its card is the
// caller's and still taking files, and the file is on the card's list, at the
// size it was listed, and not already in. The server then names the upload
// "{card}/{random}", so a card's audio shares one prefix in storage.
//
// Rejections are 4xx other than 409 and 423, which tus clients don't retry.
func (h *handlers) beforeFileUpload(hook tushandler.HookEvent) (tushandler.HTTPResponse, tushandler.FileInfoChanges, error) {
	var (
		resp tushandler.HTTPResponse
		none tushandler.FileInfoChanges
	)
	ctx, info := hook.Context, hook.Upload
	me, _ := ctx.Value(userKey{}).(db.User)
	ref, cardPath := info.MetaData[metaReference], db.CardPath(info.MetaData[metaPath])
	if ref == "" || cardPath == "" || info.SizeIsDeferred {
		return resp, none, tushandler.NewError("ERR_FILE_UNNAMED",
			"an upload needs the card reference, the file's path on the card and its length", http.StatusBadRequest)
	}

	card, err := h.store.GetUpload(ctx, ref)
	switch {
	case errors.Is(err, db.ErrNotFound) || (err == nil && !canSee(me, card)):
		return resp, none, tushandler.NewError("ERR_NO_SUCH_CARD", "no such card", http.StatusNotFound)
	case err != nil:
		return resp, none, err
	case !transferring(card.Status):
		return resp, none, tushandler.NewError("ERR_CARD_RECEIVED",
			"this card has been received and isn't taking files", http.StatusUnprocessableEntity)
	}

	file, err := h.store.GetAudioFile(ctx, ref, db.AudioFileID(ref, cardPath))
	switch {
	case errors.Is(err, db.ErrNotFound) || (err == nil && file.StatusDetail == db.AudioDetailNotOnCard):
		return resp, none, tushandler.NewError("ERR_FILE_NOT_ON_CARD",
			"that file isn't on the card's list; choose the card again", http.StatusBadRequest)
	case err != nil:
		return resp, none, err
	case file.SizeBytes != info.Size:
		return resp, none, tushandler.NewError("ERR_FILE_SIZE",
			"that file isn't the size it was when the card was read; choose the card again", http.StatusBadRequest)
	case received(file):
		return resp, none, tushandler.NewError("ERR_FILE_RECEIVED",
			"that file is already in", http.StatusUnprocessableEntity)
	}

	return resp, tushandler.FileInfoChanges{
		ID: storagePrefix(ref) + "/" + rand.Text(),
		MetaData: tushandler.MetaData{
			metaReference: ref, metaPath: cardPath,
			metaAudioFile: file.ID, metaUserID: me.ID,
		},
	}, nil
}

// afterFileUpload records a file whose last byte has landed. It runs before
// the browser is told the upload succeeded, so by the time the browser asks,
// the card's counts include the file.
//
// If it fails, the browser sees an error for a file that is in fact stored.
// The file stays pending, and is sent again the next time the card is.
func (h *handlers) afterFileUpload(hook tushandler.HookEvent) (tushandler.HTTPResponse, error) {
	ctx, info := hook.Context, hook.Upload
	ref, fileID := info.MetaData[metaReference], info.MetaData[metaAudioFile]
	now := h.stamp()
	_, err := h.store.UpdateAudioFile(ctx, ref, fileID, func(f *db.AudioFile) error {
		// A file already in keeps its first copy; one taken off the card's
		// list while it was being sent stays off it.
		if received(*f) || f.StatusDetail == db.AudioDetailNotOnCard {
			return nil
		}
		f.Status, f.StatusDetail = db.AudioUploaded, ""
		f.UploadedAt, f.BlobName = &now, storage.Name(info.ID)
		return nil
	})
	if err == nil {
		_, err = h.tallyFiles(ctx, ref)
	}
	if err != nil {
		h.log.Error("recording a received file", "upload", info.ID, "err", err)
	}
	return tushandler.HTTPResponse{}, err
}

// received reports whether a file's audio is in storage: uploaded, and maybe
// analyzed since.
func received(f db.AudioFile) bool {
	return f.Status == db.AudioUploaded || f.Status == db.AudioAnalyzing || f.Status == db.AudioAnalyzed
}

// storagePrefix is a card reference as an upload id can spell it. References
// are built from a recorder id a coordinator typed, and an upload id has to be
// URL-safe, so anything else becomes "_". It only groups a card's files; the
// card itself is found through the upload's metadata.
func storagePrefix(ref string) string {
	return strings.Map(func(r rune) rune {
		switch {
		case r >= 'a' && r <= 'z', r >= 'A' && r <= 'Z', r >= '0' && r <= '9', r == '-', r == '_', r == '.':
			return r
		}
		return '_'
	}, ref)
}
