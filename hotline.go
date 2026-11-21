// Package hotline is a support desk with no infrastructure of its own. A user writes a message, with files if they need
// to, and the desk brings it to the operator on shift who has the ticket, through that operator's adapter: a Telegram
// chat, Slack, email, whatever a short adapter speaks. An operator's answer comes back through Desk.Answer and goes to
// every user listener: the host app's inbox, a webhook, an email. Where tickets live is a Store; Memory is one for tests
// and demos.
package hotline

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"slices"
	"strings"
	"time"
	"unicode/utf8"
)

// Side is who wrote a message.
type Side string

const (
	UserSide     Side = "user"
	OperatorSide Side = "operator"
)

// Ticket is one conversation between a user and support. The store gives it its ID.
type Ticket struct {
	ID   string
	User string
	// Unanswered counts the user's messages since an operator last wrote.
	Unanswered int
	Closed     bool
	// Operator is who has the ticket: what users write in it goes to them. Empty until its first message is delivered.
	Operator string
}

// Message is what one side said. A message may close its ticket, with words or without: nothing more goes in after it.
type Message struct {
	ID       string
	TicketID string
	From     Side
	Text     string
	Files    []File
	Closed   bool
	At       time.Time
}

// Draft is what one side writes: words, files, or both, and whether it closes the ticket. The desk makes a Message of it.
type Draft struct {
	Text  string
	Files [][]byte
	Close bool
}

// File is an attachment. Its type is sniffed from the bytes, never taken from the sender, and it has no name: a name is
// one more thing the sender controls.
type File struct {
	Type string
	Data []byte
}

// Listener brings a message to one side. Deliver returns nil only once the message is safely there: an error means it
// goes again later. So a message may come more than once, and its ID tells the copies apart.
type Listener interface {
	Deliver(ctx context.Context, t Ticket, m Message) error
}

// ListenerFunc lets a plain function be a Listener.
type ListenerFunc func(ctx context.Context, t Ticket, m Message) error

func (f ListenerFunc) Deliver(ctx context.Context, t Ticket, m Message) error { return f(ctx, t, m) }

// Operators is an operator adapter: it brings users' messages to the operators it serves, each a chat or a mailbox. The
// desk gives it only the messages of tickets whose operator it serves.
type Operators interface {
	Listener
	// Serves says an operator is the adapter's, and can take messages now. The desk asks it while the store holds a
	// ticket, so it answers at once.
	Serves(operator string) bool
}

// Operator makes a listener the adapter of one operator with no shifts of their own, as a mailbox: they are always on
// shift. Run opens their shift at start, and Desk.Shift does not close it.
func Operator(name string, listener Listener) Operators { return single{name, listener} }

type single struct {
	name string
	Listener
}

func (s single) Serves(operator string) bool { return operator == s.name }

// Store keeps tickets and the messages still to deliver. Each method is atomic. The desk's rules come as callbacks, and
// a store keeps only these of its own, which package storetest checks:
//   - a ticket is open until it is closed: Open and Shift count those, and Assign shows each operator's;
//   - a callback that refuses leaves the ticket and the messages as they were;
//   - Next hides what it gives, and gives the messages of each side of a ticket in the order they came.
type Store interface {
	// Open saves t with its first message once check agrees with how many tickets t.User has open, and answers the
	// message with its ID and TicketID.
	Open(ctx context.Context, t Ticket, m Message, check func(open int) error) (Message, error)
	// Add saves m into ticket m.TicketID once change has agreed to the ticket as it stands and changed it, and answers m
	// with its ID. A ticket that does not exist is ErrNoTicket.
	Add(ctx context.Context, m Message, change func(t *Ticket) error) (Message, error)
	// Next answers the oldest message not yet delivered whose time has come and which is the first not delivered from
	// its side of its ticket, with the ticket, and hides it for hide: one not Delivered by then comes again. ok is false
	// when nothing waits.
	Next(ctx context.Context, hide time.Duration) (t Ticket, m Message, ok bool, err error)
	// Delivered says a message reached every listener. The store may forget it then.
	Delivered(ctx context.Context, messageID string) error
	// Shift opens or closes an operator's shift, and answers how many open tickets the operator has.
	Shift(ctx context.Context, operator string, on bool) (open int, err error)
	// Assign gives ticket ticketID the operator choose picks, and answers the ticket. choose sees the ticket as it stands
	// and every operator on shift with how many open tickets they have. A ticket that does not exist is ErrNoTicket.
	Assign(ctx context.Context, ticketID string, choose func(t Ticket, onShift map[string]int) (string, error)) (Ticket, error)
}

// Limits keep a message to what fits an operator's chat, and one user from burying the operators.
type Limits struct {
	Text        int      // characters in a message
	Files       int      // files in a message
	FileSize    int      // bytes in one file
	Types       []string // file types taken, as http.DetectContentType names them
	OpenTickets int      // tickets one user has open at once
	Unanswered  int      // messages a user writes in a row before an operator answers
}

var (
	// ErrBadMessage is a draft the desk does not take. Each of the errors after it says why, and is one.
	ErrBadMessage = errors.New("hotline: the message is not taken")
	ErrEmpty      = fmt.Errorf("%w: it has no words and no files", ErrBadMessage)
	ErrNotText    = fmt.Errorf("%w: its words are not UTF-8", ErrBadMessage)
	ErrLongText   = fmt.Errorf("%w: its words are longer than Limits.Text", ErrBadMessage)
	ErrManyFiles  = fmt.Errorf("%w: it has more files than Limits.Files", ErrBadMessage)
	ErrLargeFile  = fmt.Errorf("%w: a file is empty or larger than Limits.FileSize", ErrBadMessage)
	ErrFileType   = fmt.Errorf("%w: a file is of a type not in Limits.Types", ErrBadMessage)

	ErrNoTicket    = errors.New("hotline: no such ticket")
	ErrClosed      = errors.New("hotline: the ticket is closed")
	ErrUnanswered  = errors.New("hotline: too many messages wait for an answer")
	ErrManyTickets = errors.New("hotline: too many open tickets")
	// ErrNobody means nobody can take a message now: no operator is on shift, or a listener says so. The message waits
	// and goes again after Retry, and the desk does not log it as a failure.
	ErrNobody = errors.New("hotline: nobody to take the message now")
)

// Desk takes messages from both sides: a user's goes to the adapter of its ticket's operator, an operator's to every user
// listener.
type Desk struct {
	Limits Limits
	// Retry is how long a message the desk takes stays its own to deliver. Listeners get nine tenths of it, so no other
	// process takes the message while one still works on it. A message that failed, or that nobody could take, goes again
	// after it, and Run looks for such messages that often. Set it and Limits before Run.
	// ponytail: the pause after a failure is the time listeners get; a store method to postpone a failed message if
	// uploads need far longer than a retry should wait.
	Retry time.Duration

	store     Store
	operators []Operators
	users     []Listener
	wake      chan struct{}
}

// New makes a desk whose limits fit a Telegram message and a few screenshots. Operators hear what users write, users
// hear what operators write. Nothing is delivered until Run.
func New(store Store, operators []Operators, users []Listener) *Desk {
	return &Desk{
		Limits: Limits{
			Text: 2000, Files: 3, FileSize: 8 << 20, OpenTickets: 5, Unanswered: 5,
			Types: []string{"image/png", "image/jpeg", "application/pdf", "text/plain; charset=utf-8"},
		},
		Retry: time.Minute,
		store: store, operators: operators, users: users,
		wake: make(chan struct{}, 1),
	}
}

// Write is the user's side: it opens a ticket when ticketID is empty, or adds to one of the user's own. Someone else's
// ticket is ErrNoTicket, as if there were none. user is whoever the host app signed in, and is never empty.
func (d *Desk) Write(ctx context.Context, user, ticketID string, draft Draft) (Message, error) {
	if user == "" {
		return Message{}, errors.New("hotline: no user")
	}
	m, err := d.message(draft, UserSide, ticketID)
	if err != nil {
		return m, err
	}
	closes := m.Closed
	if ticketID == "" {
		m, err = d.store.Open(ctx, Ticket{User: user, Unanswered: 1, Closed: closes}, m, func(open int) error {
			if open >= d.Limits.OpenTickets {
				return ErrManyTickets
			}
			return nil
		})
	} else {
		m, err = d.store.Add(ctx, m, func(t *Ticket) error {
			switch {
			case t.User != user:
				return ErrNoTicket
			case t.Closed:
				return ErrClosed
			case t.Unanswered >= d.Limits.Unanswered:
				return ErrUnanswered
			}
			t.Unanswered++
			t.Closed = closes
			return nil
		})
	}
	if err == nil {
		d.poke()
	}
	return m, err
}

// Answer is the operator's side: an operator adapter calls it with what an operator wrote in a ticket.
func (d *Desk) Answer(ctx context.Context, ticketID string, draft Draft) (Message, error) {
	m, err := d.message(draft, OperatorSide, ticketID)
	if err != nil {
		return m, err
	}
	closes := m.Closed
	m, err = d.store.Add(ctx, m, func(t *Ticket) error {
		if t.Closed {
			return ErrClosed
		}
		t.Unanswered = 0
		t.Closed = closes
		return nil
	})
	if err == nil {
		d.poke()
	}
	return m, err
}

// message checks a draft against the limits and makes the message: who wrote it, when, and each file's sniffed type.
func (d *Desk) message(draft Draft, from Side, ticketID string) (Message, error) {
	m := Message{TicketID: ticketID, From: from, Text: strings.TrimSpace(draft.Text), Closed: draft.Close, At: time.Now()}
	switch {
	case m.Text == "" && len(draft.Files) == 0 && !m.Closed:
		return m, ErrEmpty
	case !utf8.ValidString(m.Text):
		return m, ErrNotText
	case utf8.RuneCountInString(m.Text) > d.Limits.Text:
		return m, ErrLongText
	case len(draft.Files) > d.Limits.Files:
		return m, ErrManyFiles
	}
	for _, data := range draft.Files {
		file := File{Type: http.DetectContentType(data), Data: data}
		switch {
		case len(data) == 0 || len(data) > d.Limits.FileSize:
			return m, ErrLargeFile
		case !slices.Contains(d.Limits.Types, file.Type):
			return m, ErrFileType
		}
		m.Files = append(m.Files, file)
	}
	return m, nil
}

// Shift opens or closes an operator's shift, as their adapter heard it, and answers how many open tickets they have.
// Only an operator an adapter serves opens a shift; any shift can be closed. This is where the rule of shifts lives:
//   - users' messages go only to operators on shift whom an adapter serves;
//   - a ticket goes to the one with the fewest open tickets, closed ones counting for nobody, and stays with them while
//     they are on shift and served; otherwise it moves the next time its user writes, and the new operator does not see
//     the earlier messages;
//   - with nobody to take them, messages wait.
//
// The name is the adapter's, as "telegram:42", and unique among adapters. An operator made with Operator is always on
// shift.
func (d *Desk) Shift(ctx context.Context, operator string, on bool) (int, error) {
	adapter := d.adapter(operator)
	if _, always := adapter.(single); always && !on {
		return 0, fmt.Errorf("hotline: operator %q is always on shift", operator)
	}
	if on && adapter == nil {
		return 0, fmt.Errorf("hotline: no adapter serves operator %q", operator)
	}
	return d.store.Shift(ctx, operator, on)
}

// openAlways opens the shifts of the operators made with Operator, and answers whether they all are open.
func (d *Desk) openAlways(ctx context.Context) bool {
	for _, adapter := range d.operators {
		if one, always := adapter.(single); always {
			if _, err := d.store.Shift(ctx, one.name, true); err != nil {
				slog.Error("hotline: shift not opened, it is tried again later", "operator", one.name, "error", err)
				return false
			}
		}
	}
	return true
}

// adapter is the one that serves an operator, or nil.
func (d *Desk) adapter(operator string) Operators {
	for _, adapter := range d.operators {
		if adapter.Serves(operator) {
			return adapter
		}
	}
	return nil
}

// choose is the rule of shifts for Store.Assign: see Shift.
func (d *Desk) choose(t Ticket, onShift map[string]int) (string, error) {
	if _, on := onShift[t.Operator]; on && d.adapter(t.Operator) != nil {
		return t.Operator, nil
	}
	chosen := ""
	for operator, open := range onShift {
		if d.adapter(operator) == nil {
			continue
		}
		if chosen == "" || open < onShift[chosen] || open == onShift[chosen] && operator < chosen {
			chosen = operator
		}
	}
	if chosen == "" {
		return "", ErrNobody
	}
	return chosen, nil
}

// poke wakes Run for a message just written.
func (d *Desk) poke() {
	select {
	case d.wake <- struct{}{}:
	default:
	}
}

// Run delivers messages until ctx ends: each right after it is written, and one a listener failed after Retry. First it
// opens the shifts of the operators who are always on. Several processes may run it over one store, since Next hides
// what one of them took.
func (d *Desk) Run(ctx context.Context) {
	opened := false
	for {
		if !opened {
			opened = d.openAlways(ctx)
		}
		for ctx.Err() == nil && d.deliverNext(ctx) {
		}
		select {
		case <-ctx.Done():
			return
		case <-d.wake:
		case <-time.After(d.Retry):
		}
	}
}

// deliverNext brings the next message to the other side, and answers whether there was one: a user's message to the
// adapter of the ticket's operator, an operator's message to every user listener.
// ponytail: delivery is per message, so when one of two user listeners fails, the other gets it again; per-listener state
// in the store if duplicates matter more than the ID that tells them apart.
func (d *Desk) deliverNext(ctx context.Context) bool {
	deadline := time.Now().Add(d.Retry * 9 / 10) // counted before Next hides the message, so it ends first
	t, m, ok, err := d.store.Next(ctx, d.Retry)
	if err != nil {
		slog.Error("hotline: next message not read", "error", err)
		return false
	}
	if !ok {
		return false
	}
	listeners := d.users
	if m.From == UserSide {
		assigned, err := d.store.Assign(ctx, t.ID, d.choose)
		if err != nil {
			if !errors.Is(err, ErrNobody) {
				slog.Error("hotline: ticket not given to an operator, it goes again later", "ticket", t.ID, "error", err)
			}
			return true
		}
		adapter := d.adapter(assigned.Operator)
		if adapter == nil { // the adapter stopped serving them since Assign
			slog.Warn("hotline: no adapter serves the operator, it goes again later", "ticket", t.ID, "operator", assigned.Operator)
			return true
		}
		t, listeners = assigned, []Listener{adapter}
	}
	delivering, cancel := context.WithDeadline(ctx, deadline)
	defer cancel()
	for _, listener := range listeners {
		if err := listener.Deliver(delivering, t, m); err != nil {
			if !errors.Is(err, ErrNobody) {
				slog.Warn("hotline: message not delivered, it goes again later", "ticket", t.ID, "message", m.ID, "error", err)
			}
			return true
		}
	}
	if err := d.store.Delivered(ctx, m.ID); err != nil {
		slog.Error("hotline: delivered message not marked, it goes again", "message", m.ID, "error", err)
	}
	return true
}
