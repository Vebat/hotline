package hotline

import (
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"mime/multipart"
	"net/http"
)

// refusals are the answers to what Write refuses.
var refusals = []struct {
	err    error
	status int
	code   string
}{
	{ErrEmpty, http.StatusBadRequest, "empty"},
	{ErrNotText, http.StatusBadRequest, "not_text"},
	{ErrLongText, http.StatusBadRequest, "too_long"},
	{ErrManyFiles, http.StatusBadRequest, "too_many_files"},
	{ErrLargeFile, http.StatusBadRequest, "file_too_large"},
	{ErrFileType, http.StatusBadRequest, "file_type"},
	{ErrNoTicket, http.StatusNotFound, "no_ticket"},
	{ErrClosed, http.StatusConflict, "closed"},
	{ErrUnanswered, http.StatusConflict, "wait_for_answer"},
	{ErrManyTickets, http.StatusConflict, "too_many_tickets"},
}

// Handler is the one endpoint users write through: a POST of multipart/form-data with the words in "text", files in
// "file", up to Limits.Files of them, "ticket" to write into one already open, and "close=true" to close it with this
// message. It answers 201 with the IDs {"ticket", "message"}, or an error {"error": code}. who names the user as the
// host app's sign-in does: an error or an empty name answers 401. Operators' answers reach users through user
// listeners, not through here.
func (d *Desk) Handler(who func(*http.Request) (string, error)) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		user, err := who(r)
		if err != nil || user == "" {
			reply(w, http.StatusUnauthorized, map[string]string{"error": "unauthorized"})
			return
		}
		most := int64(d.Limits.Files)*int64(d.Limits.FileSize) + 1<<20 // and a megabyte for the words and the form
		r.Body = http.MaxBytesReader(w, r.Body, most)
		if err := r.ParseMultipartForm(most); err != nil {
			var tooBig *http.MaxBytesError
			if errors.As(err, &tooBig) {
				reply(w, http.StatusRequestEntityTooLarge, map[string]string{"error": "too_large"})
				return
			}
			reply(w, http.StatusBadRequest, map[string]string{"error": "bad_form"})
			return
		}
		defer r.MultipartForm.RemoveAll()

		draft := Draft{Text: r.PostFormValue("text"), Close: r.PostFormValue("close") == "true"}
		for _, header := range r.MultipartForm.File["file"] {
			data, err := read(header)
			if err != nil {
				reply(w, http.StatusBadRequest, map[string]string{"error": "bad_form"})
				return
			}
			draft.Files = append(draft.Files, data)
		}
		m, err := d.Write(r.Context(), user, r.PostFormValue("ticket"), draft)
		if err != nil {
			for _, refused := range refusals {
				if errors.Is(err, refused.err) {
					reply(w, refused.status, map[string]string{"error": refused.code})
					return
				}
			}
			slog.Error("hotline: message not written", "error", err)
			reply(w, http.StatusInternalServerError, map[string]string{"error": "unavailable"})
			return
		}
		reply(w, http.StatusCreated, map[string]string{"ticket": m.TicketID, "message": m.ID})
	})
}

func read(header *multipart.FileHeader) ([]byte, error) {
	file, err := header.Open()
	if err != nil {
		return nil, err
	}
	defer file.Close()
	return io.ReadAll(file)
}

func reply(w http.ResponseWriter, status int, body map[string]string) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	if err := json.NewEncoder(w).Encode(body); err != nil {
		slog.Warn("hotline: answer not written", "error", err)
	}
}
