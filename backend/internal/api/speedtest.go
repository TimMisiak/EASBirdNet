package api

import (
	"errors"
	"io"
	"net/http"
	"time"

	"github.com/ngaitonde/EASBirdNet/backend/internal/db"
)

// speedTestMaxBytes is the most one speed-test request may carry. The browser
// sends 8 MB at a time (js/speed-test.js); this leaves room without letting
// one request run on forever.
const speedTestMaxBytes = 16 << 20

// speedTestDeadline bounds how long one request may take to arrive: 16 MB at
// ~1 Mbit/s. The server has no ReadTimeout (cmd/server), so this is the one
// thing that ends a body that trickles in.
const speedTestDeadline = 2 * time.Minute

// speedTest reads the body and throws it away. It is the volunteer's upload
// speed test: the same route through the ingress a card's audio takes, with
// nothing written to storage or the database, so a slow line can be told
// apart from a slow card reader or a slow store behind tusd. The session check
// is the only read it makes.
func (h *handlers) speedTest(w http.ResponseWriter, r *http.Request, _ db.User) {
	start := time.Now()
	// Best effort: a writer that can't set a deadline still has the size cap.
	_ = http.NewResponseController(w).SetReadDeadline(start.Add(speedTestDeadline))
	n, err := io.Copy(io.Discard, http.MaxBytesReader(w, r.Body, speedTestMaxBytes))
	if err != nil {
		var tooBig *http.MaxBytesError
		if errors.As(err, &tooBig) {
			h.problem(w, http.StatusRequestEntityTooLarge, "a speed test sends at most 16 MB at a time")
			return
		}
		// The browser gave up, or the line dropped: nobody is waiting for an answer.
		h.problem(w, http.StatusBadRequest, "the test upload didn't arrive")
		return
	}
	h.json(w, http.StatusOK, map[string]any{"bytes": n, "ms": time.Since(start).Milliseconds()})
}
