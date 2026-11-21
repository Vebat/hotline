// Package storetest checks that a hotline.Store keeps what the desk relies on. A store runs it from its own tests:
//
//	func TestPostgres(t *testing.T) {
//		storetest.Run(t, func() hotline.Store { return newPostgres(t) })
//	}
//
// fresh makes an empty store for each case.
package storetest

import (
	"context"
	"errors"
	"maps"
	"sync"
	"testing"
	"time"

	"github.com/Vebat/hotline"
)

var refused = errors.New("refused")

// Run checks a store through its interface only.
func Run(t *testing.T, fresh func() hotline.Store) {
	ctx := context.Background()
	user := func(text string) hotline.Message {
		return hotline.Message{From: hotline.UserSide, Text: text, At: time.Now()}
	}
	open := func(t *testing.T, s hotline.Store, who, text string) hotline.Message {
		t.Helper()
		m, err := s.Open(ctx, hotline.Ticket{User: who, Unanswered: 1}, user(text), func(int) error { return nil })
		if err != nil || m.ID == "" || m.TicketID == "" {
			t.Fatalf("opened %+v, %v", m, err)
		}
		return m
	}
	closeTicket := func(t *testing.T, s hotline.Store, ticketID string) {
		t.Helper()
		closing := hotline.Message{TicketID: ticketID, From: hotline.OperatorSide, Closed: true}
		if _, err := s.Add(ctx, closing, func(t *hotline.Ticket) error { t.Closed = true; return nil }); err != nil {
			t.Fatal(err)
		}
	}
	// drain answers the texts Next gives, delivering each, until none is due.
	drain := func(t *testing.T, s hotline.Store) []string {
		t.Helper()
		var texts []string
		for {
			_, m, ok, err := s.Next(ctx, time.Hour)
			if err != nil {
				t.Fatal(err)
			}
			if !ok {
				return texts
			}
			if err := s.Delivered(ctx, m.ID); err != nil {
				t.Fatal(err)
			}
			texts = append(texts, m.Text)
		}
	}
	same := func(t *testing.T, got, want []string) {
		t.Helper()
		if len(got) != len(want) {
			t.Fatalf("got %q, want %q", got, want)
		}
		for i := range got {
			if got[i] != want[i] {
				t.Fatalf("got %q, want %q", got, want)
			}
		}
	}

	t.Run("open counts a user's open tickets and a refusal leaves nothing", func(t *testing.T) {
		s := fresh()
		first := open(t, s, "ann", "one")
		open(t, s, "bob", "someone else's")
		closeTicket(t, s, first.TicketID)
		open(t, s, "ann", "two")
		seen := -1
		_, err := s.Open(ctx, hotline.Ticket{User: "ann"}, user("refused"), func(n int) error { seen = n; return refused })
		if !errors.Is(err, refused) || seen != 1 {
			t.Fatalf("check saw %d open tickets and answered %v; want 1 and the refusal", seen, err)
		}
		same(t, drain(t, s), []string{"one", "someone else's", "", "two"})
	})

	t.Run("add changes the ticket as it stands and a refusal leaves it", func(t *testing.T) {
		s := fresh()
		first := open(t, s, "ann", "one")
		if _, err := s.Add(ctx, hotline.Message{TicketID: "no such"}, func(*hotline.Ticket) error { return nil }); !errors.Is(err, hotline.ErrNoTicket) {
			t.Fatalf("a ticket that does not exist answered %v", err)
		}
		next := user("two")
		next.TicketID = first.TicketID
		added, err := s.Add(ctx, next, func(t *hotline.Ticket) error { t.Unanswered = 7; return nil })
		if err != nil || added.ID == "" || added.ID == first.ID {
			t.Fatalf("added %+v, %v", added, err)
		}
		refusedToo := user("refused")
		refusedToo.TicketID = first.TicketID
		_, err = s.Add(ctx, refusedToo, func(t *hotline.Ticket) error { t.Unanswered, t.Closed = 99, true; return refused })
		if !errors.Is(err, refused) {
			t.Fatalf("a refusal answered %v", err)
		}
		var seen hotline.Ticket
		last := user("three")
		last.TicketID = first.TicketID
		if _, err := s.Add(ctx, last, func(t *hotline.Ticket) error { seen = *t; return nil }); err != nil {
			t.Fatal(err)
		}
		if seen.ID != first.TicketID || seen.User != "ann" || seen.Unanswered != 7 || seen.Closed {
			t.Fatalf("the ticket stands as %+v", seen)
		}
		same(t, drain(t, s), []string{"one", "two", "three"})
	})

	t.Run("add is atomic", func(t *testing.T) {
		s := fresh()
		first := open(t, s, "ann", "one")
		var wait sync.WaitGroup
		for range 20 {
			wait.Add(1)
			go func() {
				defer wait.Done()
				m := user("again")
				m.TicketID = first.TicketID
				if _, err := s.Add(ctx, m, func(t *hotline.Ticket) error { t.Unanswered++; return nil }); err != nil {
					t.Error(err)
				}
			}()
		}
		wait.Wait()
		check := user("check")
		check.TicketID = first.TicketID
		seen := 0
		if _, err := s.Add(ctx, check, func(t *hotline.Ticket) error { seen = t.Unanswered; return nil }); err != nil {
			t.Fatal(err)
		}
		if seen != 21 {
			t.Fatalf("20 adds at once left Unanswered at %d, want 21", seen)
		}
		if got := len(drain(t, s)); got != 22 {
			t.Fatalf("20 adds at once kept %d messages, want 22 with the first and the check", got)
		}
	})

	t.Run("next hides what it gives until delivered", func(t *testing.T) {
		s := fresh()
		if _, _, ok, err := s.Next(ctx, time.Hour); ok || err != nil {
			t.Fatalf("an empty store gave a message, %v", err)
		}
		first := open(t, s, "ann", "one")
		open(t, s, "bob", "two")
		ticket, m, ok, err := s.Next(ctx, 0)
		if !ok || err != nil || m.ID != first.ID || m.Text != "one" || ticket.ID != first.TicketID || ticket.User != "ann" {
			t.Fatalf("next gave %+v of %+v, %v, %v", m, ticket, ok, err)
		}
		if _, again, _, _ := s.Next(ctx, time.Hour); again.ID != first.ID {
			t.Fatalf("a message hidden for no time did not come first again, got %q", again.Text)
		}
		if err := s.Delivered(ctx, first.ID); err != nil {
			t.Fatal(err)
		}
		same(t, drain(t, s), []string{"two"})
	})

	t.Run("next keeps the order of each side of a ticket", func(t *testing.T) {
		s := fresh()
		first := open(t, s, "ann", "one")
		for _, m := range []hotline.Message{user("two"), {From: hotline.OperatorSide, Text: "answer"}} {
			m.TicketID = first.TicketID
			if _, err := s.Add(ctx, m, func(*hotline.Ticket) error { return nil }); err != nil {
				t.Fatal(err)
			}
		}
		var given []string
		for {
			_, m, ok, err := s.Next(ctx, time.Hour)
			if err != nil {
				t.Fatal(err)
			}
			if !ok {
				break
			}
			given = append(given, m.Text)
		}
		same(t, given, []string{"one", "answer"}) // "two" waits until "one" is delivered
		if err := s.Delivered(ctx, first.ID); err != nil {
			t.Fatal(err)
		}
		same(t, drain(t, s), []string{"two"})
	})

	t.Run("shifts and assignment count open tickets", func(t *testing.T) {
		s := fresh()
		first := open(t, s, "ann", "one")
		second := open(t, s, "bob", "two")
		if n, err := s.Shift(ctx, "anna", true); n != 0 || err != nil {
			t.Fatalf("anna opened a shift with %d tickets, %v", n, err)
		}
		if _, err := s.Assign(ctx, "no such", func(hotline.Ticket, map[string]int) (string, error) { return "anna", nil }); !errors.Is(err, hotline.ErrNoTicket) {
			t.Fatalf("a ticket that does not exist answered %v", err)
		}
		assign := func(ticketID, operator string, want map[string]int) hotline.Ticket {
			t.Helper()
			var seen map[string]int
			got, err := s.Assign(ctx, ticketID, func(t hotline.Ticket, onShift map[string]int) (string, error) {
				seen = onShift
				return operator, nil
			})
			if err != nil || got.Operator != operator || !maps.Equal(seen, want) {
				t.Fatalf("assigned %+v, %v; choose saw %v, want %v", got, err, seen, want)
			}
			return got
		}
		assign(first.TicketID, "anna", map[string]int{"anna": 0})
		s.Shift(ctx, "boris", true)
		assign(second.TicketID, "boris", map[string]int{"anna": 1, "boris": 0})

		_, err := s.Assign(ctx, second.TicketID, func(t hotline.Ticket, _ map[string]int) (string, error) {
			if t.Operator != "boris" {
				return "", errors.New("choose did not see the ticket as it stands")
			}
			return "", refused
		})
		if !errors.Is(err, refused) {
			t.Fatalf("a refusal answered %v", err)
		}
		if ticket, _, _, _ := s.Next(ctx, time.Hour); ticket.Operator != "anna" {
			t.Fatalf("next gave the ticket of %q, want anna", ticket.Operator)
		}

		closeTicket(t, s, first.TicketID)
		if n, _ := s.Shift(ctx, "anna", false); n != 0 {
			t.Fatalf("anna left a shift with %d open tickets, but hers is closed", n)
		}
		if n, _ := s.Shift(ctx, "boris", true); n != 1 {
			t.Fatalf("boris has %d open tickets, want 1: a refused choice changed his ticket", n)
		}
		assign(second.TicketID, "boris", map[string]int{"boris": 1})
	})
}
