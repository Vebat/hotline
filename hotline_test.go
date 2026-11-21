package hotline_test

import (
	"bytes"
	"context"
	"errors"
	"mime/multipart"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/Vebat/hotline"
)

var png = []byte("\x89PNG\r\n\x1a\n")

// start runs a desk until the test ends.
func start(t *testing.T, desk *hotline.Desk) context.Context {
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	go desk.Run(ctx)
	return ctx
}

// receive waits for what a listener got.
func receive[T any](t *testing.T, from <-chan T) T {
	t.Helper()
	select {
	case got := <-from:
		return got
	case <-time.After(2 * time.Second):
		t.Fatal("nothing came")
		panic("unreachable")
	}
}

func refused(t *testing.T, err, want error) {
	t.Helper()
	if !errors.Is(err, want) {
		t.Fatalf("got %v, want %v", err, want)
	}
}

func TestDesk(t *testing.T) {
	toOperators := make(chan hotline.Message, 10)
	var operatorsDown atomic.Bool
	operatorsDown.Store(true)
	support := hotline.Operator("support", hotline.ListenerFunc(func(_ context.Context, _ hotline.Ticket, m hotline.Message) error {
		if operatorsDown.CompareAndSwap(true, false) {
			return errors.New("the operators' chat is down")
		}
		toOperators <- m
		return nil
	}))
	inbox, webhook := make(chan hotline.Message, 10), make(chan hotline.Message, 10)
	var webhookDown atomic.Bool
	webhookDown.Store(true)
	users := []hotline.Listener{
		hotline.ListenerFunc(func(_ context.Context, ticket hotline.Ticket, m hotline.Message) error {
			if ticket.User != "ann" {
				t.Errorf("an answer went to %q", ticket.User)
			}
			inbox <- m
			return nil
		}),
		hotline.ListenerFunc(func(_ context.Context, _ hotline.Ticket, m hotline.Message) error {
			if webhookDown.CompareAndSwap(true, false) {
				return errors.New("the webhook is down")
			}
			webhook <- m
			return nil
		}),
	}
	desk := hotline.New(&hotline.Memory{}, []hotline.Operators{support}, users)
	desk.Retry = 20 * time.Millisecond
	desk.Limits.Unanswered, desk.Limits.OpenTickets = 2, 1
	ctx := start(t, desk) // support is one operator, so always on shift: nobody opens it
	if _, err := desk.Shift(ctx, "support", false); err == nil {
		t.Fatal("an operator who is always on shift went off it")
	}

	first, err := desk.Write(ctx, "ann", "", hotline.Draft{Text: "  it broke  ", Files: [][]byte{png}})
	if err != nil || first.TicketID != "1" {
		t.Fatalf("opened %+v, %v", first, err)
	}
	if got := receive(t, toOperators); got.ID != first.ID || got.Text != "it broke" || got.Files[0].Type != "image/png" {
		t.Fatalf("once their chat was back, operators got %+v", got)
	}

	_, err = desk.Write(ctx, "bob", "1", hotline.Draft{Text: "mine now"})
	refused(t, err, hotline.ErrNoTicket)
	_, err = desk.Write(ctx, "ann", "1", hotline.Draft{Text: "hello?"})
	refused(t, err, nil)
	_, err = desk.Write(ctx, "ann", "1", hotline.Draft{Text: "anyone?"})
	refused(t, err, hotline.ErrUnanswered)
	_, err = desk.Write(ctx, "ann", "", hotline.Draft{Text: "a second ticket"})
	refused(t, err, hotline.ErrManyTickets)
	_, err = desk.Write(ctx, "ann", "", hotline.Draft{})
	refused(t, err, hotline.ErrEmpty)
	_, err = desk.Write(ctx, "ann", "1", hotline.Draft{Text: strings.Repeat("a", 2001)})
	refused(t, err, hotline.ErrLongText)
	_, err = desk.Write(ctx, "ann", "", hotline.Draft{Text: "see this", Files: [][]byte{[]byte("<!DOCTYPE html><script>alert(1)</script>")}})
	refused(t, err, hotline.ErrFileType)
	refused(t, err, hotline.ErrBadMessage) // each refusal of a draft is one

	answer, err := desk.Answer(ctx, "1", hotline.Draft{Text: "fixed", Close: true})
	refused(t, err, nil)
	if got := receive(t, webhook); got.ID != answer.ID || !got.Closed {
		t.Fatalf("once it was back, the webhook got %+v", got)
	}
	if got := receive(t, inbox); got.ID != answer.ID || got.Text != "fixed" {
		t.Fatalf("the inbox got %+v", got)
	}
	_, err = desk.Write(ctx, "ann", "1", hotline.Draft{Text: "thanks"})
	refused(t, err, hotline.ErrClosed)
	_, err = desk.Write(ctx, "ann", "", hotline.Draft{Text: "something else"})
	refused(t, err, nil)
}

// staff is an operator adapter for tests: it serves the operators marked so, and says who got what.
type staff struct {
	mutex  sync.Mutex
	served map[string]bool
	got    chan [2]string // what a user wrote, and the operator it went to
}

func (s *staff) Serves(operator string) bool {
	s.mutex.Lock()
	defer s.mutex.Unlock()
	return s.served[operator]
}

func (s *staff) serve(operator string, on bool) {
	s.mutex.Lock()
	defer s.mutex.Unlock()
	s.served[operator] = on
}

func (s *staff) Deliver(_ context.Context, t hotline.Ticket, m hotline.Message) error {
	s.got <- [2]string{m.Text, t.Operator}
	return nil
}

// watched tells when the store found nobody to give a ticket to.
type watched struct {
	hotline.Store
	nobody chan string
}

func (w watched) Assign(ctx context.Context, ticketID string, choose func(hotline.Ticket, map[string]int) (string, error)) (hotline.Ticket, error) {
	t, err := w.Store.Assign(ctx, ticketID, choose)
	if errors.Is(err, hotline.ErrNobody) {
		select {
		case w.nobody <- ticketID:
		default:
		}
	}
	return t, err
}

func TestShifts(t *testing.T) {
	team := &staff{served: map[string]bool{"anna": true, "boris": true}, got: make(chan [2]string, 10)}
	store := watched{Store: &hotline.Memory{}, nobody: make(chan string, 10)}
	desk := hotline.New(store, []hotline.Operators{team}, nil)
	desk.Retry = 20 * time.Millisecond
	ctx := start(t, desk)
	written := func(user, ticket, text string) {
		t.Helper()
		if _, err := desk.Write(ctx, user, ticket, hotline.Draft{Text: text}); err != nil {
			t.Fatal(err)
		}
	}
	wentTo := func(text, operator string) {
		t.Helper()
		if got := receive(t, team.got); got != [2]string{text, operator} {
			t.Fatalf("%q went to %q, want %q to go to %q", got[0], got[1], text, operator)
		}
	}
	shift := func(operator string, on bool) int {
		t.Helper()
		open, err := desk.Shift(ctx, operator, on)
		if err != nil {
			t.Fatal(err)
		}
		return open
	}

	if _, err := desk.Shift(ctx, "ghost", true); err == nil {
		t.Fatal("a shift opened for an operator no adapter serves")
	}
	written("ann", "", "it broke") // ticket 1
	if receive(t, store.nobody) != "1" {
		t.Fatal("the desk did not find the shift empty")
	}
	shift("anna", true)
	shift("boris", true)
	wentTo("it broke", "anna")
	written("bob", "", "where is my knife") // ticket 2
	wentTo("where is my knife", "boris")    // boris has fewer open tickets
	if _, err := desk.Answer(ctx, "2", hotline.Draft{Text: "here it is", Close: true}); err != nil {
		t.Fatal(err)
	}
	written("cat", "", "no money") // ticket 3
	wentTo("no money", "boris")    // a closed ticket counts for nobody
	written("dan", "", "trade stuck")
	wentTo("trade stuck", "anna") // one each, and anna comes first

	restarted := hotline.New(store, []hotline.Operators{team}, nil) // keeps shifts and tickets
	restarted.Retry = 20 * time.Millisecond
	start(t, restarted)
	if _, err := restarted.Write(ctx, "ann", "1", hotline.Draft{Text: "still broken"}); err != nil {
		t.Fatal(err)
	}
	wentTo("still broken", "anna") // anna keeps her ticket though boris has fewer
	if open := shift("anna", false); open != 2 {
		t.Fatalf("anna went off shift with %d open tickets, want 2", open)
	}
	written("ann", "1", "anyone?")
	wentTo("anyone?", "boris")

	team.serve("boris", false) // boris's chat is gone from the adapter
	shift("anna", true)
	written("cat", "3", "still no money")
	wentTo("still no money", "anna")
}

func TestHandler(t *testing.T) {
	desk := hotline.New(&hotline.Memory{}, nil, nil)
	handler := desk.Handler(func(r *http.Request) (string, error) {
		if r.Header.Get("User") == "" {
			return "", errors.New("not signed in")
		}
		return r.Header.Get("User"), nil
	})
	post := func(user string, fields map[string]string, files ...[]byte) string {
		var body bytes.Buffer
		form := multipart.NewWriter(&body)
		for key, value := range fields {
			form.WriteField(key, value)
		}
		for _, file := range files {
			part, _ := form.CreateFormFile("file", "../../screenshot.png")
			part.Write(file)
		}
		form.Close()
		request := httptest.NewRequest(http.MethodPost, "/support", &body)
		request.Header.Set("Content-Type", form.FormDataContentType())
		request.Header.Set("User", user)
		response := httptest.NewRecorder()
		handler.ServeHTTP(response, request)
		return http.StatusText(response.Code) + " " + strings.TrimSpace(response.Body.String())
	}
	for _, step := range []struct{ got, want string }{
		{post("ann", map[string]string{"text": "it broke"}, png), `Created {"message":"1","ticket":"1"}`},
		{post("", map[string]string{"text": "it broke"}), `Unauthorized {"error":"unauthorized"}`},
		{post("ann", nil), `Bad Request {"error":"empty"}`},
		{post("ann", map[string]string{"text": "see"}, []byte("<!DOCTYPE html>")), `Bad Request {"error":"file_type"}`},
		{post("bob", map[string]string{"ticket": "1", "text": "mine now"}), `Not Found {"error":"no_ticket"}`},
		{post("ann", map[string]string{"ticket": "1", "text": "solved", "close": "true"}), `Created {"message":"2","ticket":"1"}`},
		{post("ann", map[string]string{"ticket": "1", "text": "again"}), `Conflict {"error":"closed"}`},
		{post("ann", nil, make([]byte, 30<<20)), `Request Entity Too Large {"error":"too_large"}`},
	} {
		if step.got != step.want {
			t.Errorf("got %s, want %s", step.got, step.want)
		}
	}
	request := httptest.NewRequest(http.MethodPost, "/support", strings.NewReader("text=plain"))
	request.Header.Set("User", "ann")
	response := httptest.NewRecorder()
	handler.ServeHTTP(response, request)
	if response.Code != http.StatusBadRequest || !strings.Contains(response.Body.String(), "bad_form") {
		t.Errorf("a body that is not a form got %d %s", response.Code, response.Body)
	}
}
