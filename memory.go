package hotline

import (
	"context"
	"strconv"
	"sync"
	"time"
)

// Memory is a Store in this process's memory, for tests, demos and the first day. Its zero value is ready, and its IDs
// are 1, 2, 3...
// ponytail: it keeps every message, and Next and Assign read everything from the first; a database store before it is real.
type Memory struct {
	mutex    sync.Mutex
	tickets  []Ticket
	messages []kept
	shifts   map[string]bool
}

type kept struct {
	Message
	due       time.Time
	delivered bool
}

func (s *Memory) Open(_ context.Context, t Ticket, m Message, check func(open int) error) (Message, error) {
	s.mutex.Lock()
	defer s.mutex.Unlock()
	open := 0
	for _, other := range s.tickets {
		if other.User == t.User && !other.Closed {
			open++
		}
	}
	if err := check(open); err != nil {
		return Message{}, err
	}
	t.ID = strconv.Itoa(len(s.tickets) + 1)
	s.tickets = append(s.tickets, t)
	m.TicketID = t.ID
	return s.keep(m), nil
}

func (s *Memory) Add(_ context.Context, m Message, change func(t *Ticket) error) (Message, error) {
	s.mutex.Lock()
	defer s.mutex.Unlock()
	index, found := s.ticket(m.TicketID)
	if !found {
		return Message{}, ErrNoTicket
	}
	t := s.tickets[index] // a copy, so a change that refuses leaves the ticket as it was
	if err := change(&t); err != nil {
		return Message{}, err
	}
	s.tickets[index] = t
	return s.keep(m), nil
}

func (s *Memory) keep(m Message) Message {
	m.ID = strconv.Itoa(len(s.messages) + 1)
	s.messages = append(s.messages, kept{Message: m})
	return m
}

func (s *Memory) Next(_ context.Context, hide time.Duration) (Ticket, Message, bool, error) {
	s.mutex.Lock()
	defer s.mutex.Unlock()
	now := time.Now()
	queued := map[[2]string]bool{} // a ticket's side that has an earlier message still to deliver
	for i := range s.messages {
		next := &s.messages[i]
		queue := [2]string{next.TicketID, string(next.From)}
		if next.delivered || queued[queue] {
			continue
		}
		queued[queue] = true
		if !now.Before(next.due) {
			next.due = now.Add(hide)
			index, _ := s.ticket(next.TicketID)
			return s.tickets[index], next.Message, true, nil
		}
	}
	return Ticket{}, Message{}, false, nil
}

func (s *Memory) Delivered(_ context.Context, messageID string) error {
	s.mutex.Lock()
	defer s.mutex.Unlock()
	if index, err := strconv.Atoi(messageID); err == nil && index >= 1 && index <= len(s.messages) {
		s.messages[index-1].delivered = true
	}
	return nil
}

func (s *Memory) Shift(_ context.Context, operator string, on bool) (int, error) {
	s.mutex.Lock()
	defer s.mutex.Unlock()
	if s.shifts == nil {
		s.shifts = map[string]bool{}
	}
	s.shifts[operator] = on
	return s.open(operator), nil
}

func (s *Memory) Assign(_ context.Context, ticketID string, choose func(t Ticket, onShift map[string]int) (string, error)) (Ticket, error) {
	s.mutex.Lock()
	defer s.mutex.Unlock()
	index, found := s.ticket(ticketID)
	if !found {
		return Ticket{}, ErrNoTicket
	}
	onShift := map[string]int{}
	for operator, on := range s.shifts {
		if on {
			onShift[operator] = s.open(operator)
		}
	}
	operator, err := choose(s.tickets[index], onShift)
	if err != nil {
		return s.tickets[index], err
	}
	s.tickets[index].Operator = operator
	return s.tickets[index], nil
}

// ticket finds a ticket's place by its ID.
func (s *Memory) ticket(id string) (int, bool) {
	index, err := strconv.Atoi(id)
	return index - 1, err == nil && index >= 1 && index <= len(s.tickets)
}

// open counts an operator's open tickets.
func (s *Memory) open(operator string) int {
	open := 0
	for _, t := range s.tickets {
		if t.Operator == operator && !t.Closed {
			open++
		}
	}
	return open
}
