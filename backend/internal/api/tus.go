package api

import (
	"context"
	"crypto/rand"
	"errors"
	"fmt"
	"net/http"
	"os"
	"path"
	"strconv"
	"strings"
	"time"

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

// Upload-Metadata keys. The browser sends reference and path, and for a WAV
// it sends as FLAC, encoding and the number of samples the WAV holds; the
// server replaces the metadata when it creates the upload, so the finish hook
// only ever reads values the server wrote.
const (
	metaReference = "reference"
	metaPath      = "path"
	metaEncoding  = "encoding"
	metaSamples   = "samples"
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
		// list to the same bound, and a FLAC of the longest file it allows
		// to this one -- so it is the backstop for a length that was never on
		// one: a POST claiming more is refused before a byte is taken, and a
		// PATCH stops reading a body at it.
		MaxSize: maxFLACBytes(maxFileBytes),
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
				// A card being deleted is as good as gone: what is sent to it
				// now would land after the deleter had cleared its storage.
				case errors.Is(err, db.ErrNotFound) || (err == nil && (!canSee(me, card) || card.Status == db.StatusDeleting)):
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
// A WAV may come as FLAC instead (encoding "flac"), when the browser has
// encoded it first. Its length is then the FLAC's, which is only known once
// the encode is done, so it is held to what a FLAC of the listed WAV could be;
// what it must hold is checked when it is finished (checkFLAC).
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
	encoding, samples, problem := sentAs(info.MetaData, cardPath)
	if problem != "" {
		return resp, none, tushandler.NewError("ERR_FILE_ENCODING", problem, http.StatusBadRequest)
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
	case encoding == "" && file.SizeBytes != info.Size:
		return resp, none, tushandler.NewError("ERR_FILE_SIZE",
			"that file isn't the size it was when the card was read; choose the card again", http.StatusBadRequest)
	case encoding == db.EncodingFLAC && (info.Size < flacHeaderBytes || info.Size > maxFLACBytes(file.SizeBytes) || samples > file.SizeBytes):
		return resp, none, tushandler.NewError("ERR_FILE_SIZE",
			"that FLAC can't have been made from the file the card was read with; choose the card again", http.StatusBadRequest)
	case received(file):
		return resp, none, tushandler.NewError("ERR_FILE_RECEIVED",
			"that file is already in", http.StatusUnprocessableEntity)
	}

	meta := tushandler.MetaData{
		metaReference: ref, metaPath: cardPath,
		metaAudioFile: file.ID, metaUserID: me.ID,
	}
	if encoding != "" {
		meta[metaEncoding], meta[metaSamples] = encoding, strconv.FormatInt(samples, 10)
	}
	return resp, tushandler.FileInfoChanges{ID: storagePrefix(ref) + "/" + rand.Text(), MetaData: meta}, nil
}

// sentAs reads how an upload says its file was sent: as it was on the card
// (no encoding), or as FLAC with the number of samples per channel the WAV
// holds, which checkFLAC holds the finished FLAC to. Only a WAV is encoded:
// anything else on a card is sent as it is.
func sentAs(meta tushandler.MetaData, cardPath string) (encoding string, samples int64, problem string) {
	switch encoding = meta[metaEncoding]; encoding {
	case "":
		return "", 0, ""
	case db.EncodingFLAC:
		n, err := strconv.ParseInt(meta[metaSamples], 10, 64)
		if !strings.EqualFold(path.Ext(cardPath), ".wav") || err != nil || n <= 0 {
			return "", 0, "only a WAV is sent as FLAC, with the number of samples it holds"
		}
		return encoding, n, ""
	default:
		return "", 0, fmt.Sprintf("files aren't taken as %q", encoding)
	}
}

// afterFileUpload records a file whose last byte has landed. It runs before
// the browser is told the upload succeeded, so by the time the browser asks,
// the card's counts include the file.
//
// The card is counted up by this one file rather than recounted: a recount
// reads every file on the card, and doing that as each file lands made a
// card cost the square of its files (CARD-COUNTS.md). Only the write that
// moves the file to uploaded counts it, so a finish that runs twice counts
// once.
//
// If it fails, the browser sees an error for a file that is in fact stored.
// If the file wasn't marked, it stays pending and is sent again the next time
// the card is; if it was marked but the card wasn't counted up, the card is a
// file short until it is registered again, which recounts it -- and the
// browser registers a card again when every file went but it isn't in.
//
// A file sent as FLAC is checked first, and refused if it isn't whole
// (checkFLAC): this is the last point before the volunteer is told the file
// is safe to erase.
func (h *handlers) afterFileUpload(hook tushandler.HookEvent) (tushandler.HTTPResponse, error) {
	ctx, info := hook.Context, hook.Upload
	ref, fileID := info.MetaData[metaReference], info.MetaData[metaAudioFile]
	encoding := info.MetaData[metaEncoding]
	if encoding == db.EncodingFLAC {
		if err := h.checkFLAC(ctx, info); err != nil {
			return tushandler.HTTPResponse{}, err
		}
	}
	start, now := time.Now(), h.stamp()
	var card db.Upload
	var counted bool
	var size int64
	_, err := h.store.UpdateAudioFile(ctx, ref, fileID, func(f *db.AudioFile) error {
		// The mutate runs again when the document changed underneath it.
		counted = false
		// A file already in keeps its first copy; one taken off the card's
		// list while it was being sent stays off it.
		if received(*f) || f.StatusDetail == db.AudioDetailNotOnCard {
			return nil
		}
		f.Status, f.StatusDetail = db.AudioUploaded, ""
		f.UploadedAt, f.BlobName = &now, storage.Name(info.ID)
		f.Encoding, f.StoredBytes = encoding, info.Size
		counted, size = true, f.SizeBytes
		return nil
	})
	if err == nil && counted {
		card, err = h.countFile(ctx, ref, size)
	}
	if err != nil {
		h.log.Error("recording a received file", "upload", info.ID, "err", err)
	} else {
		// How long a file's last PATCH waited on recording it, and how far
		// along its card is: the per-file cost CARD-COUNTS.md is about.
		h.log.Info("file received", "card", ref, "upload", info.ID, "counted", counted,
			"filesUploaded", card.FilesUploaded, "fileCount", card.FileCount, "dur", time.Since(start))
	}
	return tushandler.HTTPResponse{}, err
}

// checkFLAC reads the header of a finished upload the browser sent as FLAC,
// and refuses one that doesn't hold exactly the samples the browser read off
// the card's WAV: an encode cut short. It also refuses one whose header
// doesn't say how many it holds at all -- what a stream encoder writes unless
// the browser patches the header when the encode ends -- because libsndfile,
// which BirdNET and clip.py read audio with, takes that for a file of endless
// length and can't read it.
//
// A refused upload's bytes are deleted with the refusal: no file document
// names them, so nothing else would.
func (h *handlers) checkFLAC(ctx context.Context, info tushandler.FileInfo) error {
	name := storage.Name(info.ID)
	r, err := h.files.Open(ctx, name)
	if err != nil {
		return err
	}
	si, err := readStreamInfo(r)
	r.Close()
	want, _ := strconv.ParseInt(info.MetaData[metaSamples], 10, 64)
	var problem string
	switch {
	case errors.Is(err, errNotFLAC):
		problem = "that file isn't a FLAC"
	case err != nil:
		return err
	case si.TotalSamples == 0:
		problem = "that FLAC doesn't say how long it is"
	case si.TotalSamples != want:
		problem = fmt.Sprintf("that FLAC holds %d samples, not the %d on the card", si.TotalSamples, want)
	default:
		return nil
	}
	if err := h.files.Delete(ctx, name); err != nil {
		h.log.Error("deleting a refused FLAC", "upload", info.ID, "err", err)
	}
	h.log.Warn("refused a FLAC", "card", info.MetaData[metaReference], "upload", info.ID, "path", info.MetaData[metaPath], "why", problem)
	return tushandler.NewError("ERR_FILE_FLAC", problem, http.StatusBadRequest)
}

// received reports whether a file's audio is in storage: uploaded, and maybe
// analyzed since.
func received(f db.AudioFile) bool {
	return f.Status == db.AudioUploaded || f.Status == db.AudioAnalyzing || f.Status == db.AudioAnalyzed
}

// storagePrefix is a card reference as an upload id can spell it. References
// are built from a recorder id a coordinator typed, and an upload id has to be
// URL-safe, so anything else becomes "_" -- storage.Segment, the same mapping
// a clip's name goes through, so a card's audio and its clips are always found
// under the same element. It only groups a card's files; the card itself is
// found through the upload's metadata.
func storagePrefix(ref string) string {
	return storage.Segment(ref)
}
