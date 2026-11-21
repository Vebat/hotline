package telegram_test

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"path"
	"slices"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/Vebat/hotline"
	"github.com/Vebat/hotline/telegram"
)

// fakeAPI is the Bot API for tests. getUpdates answers what operators wrote, from the offset on, as Telegram does.
// Every other call goes to calls as "chat method: words". Chat 43 takes no reactions.
type fakeAPI struct {
	mutex   sync.Mutex
	updates []string // each a message as JSON; its update_id is its place, from 1
	calls   chan string
}

// written is a message an operator wrote, as Telegram puts it in an update: repliedTo is the bot's message it answers.
func written(chat int64, text, repliedTo string) json.RawMessage {
	said := map[string]any{"message_id": 9, "text": text, "chat": map[string]int64{"id": chat}}
	if repliedTo != "" {
		said["reply_to_message"] = map[string]any{"caption": repliedTo, "from": map[string]bool{"is_bot": true}}
	}
	encoded, _ := json.Marshal(said)
	return encoded
}

func (f *fakeAPI) push(chat int64, text, repliedTo string) {
	f.mutex.Lock()
	defer f.mutex.Unlock()
	f.updates = append(f.updates, string(written(chat, text, repliedTo)))
}

// expect waits for these calls to the Bot API, in any order.
func (f *fakeAPI) expect(t *testing.T, want ...string) {
	t.Helper()
	var got []string
	for range want {
		select {
		case call := <-f.calls:
			got = append(got, call)
		case <-time.After(2 * time.Second):
			t.Fatalf("telegram got only %q, want %q", got, want)
		}
	}
	slices.Sort(got)
	slices.Sort(want)
	if !slices.Equal(got, want) {
		t.Fatalf("telegram got %q, want %q", got, want)
	}
}

func (f *fakeAPI) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	method := path.Base(r.URL.Path)
	if method == "getUpdates" {
		var asked struct {
			Offset int `json:"offset"`
		}
		json.NewDecoder(r.Body).Decode(&asked)
		for range 20 { // an empty poll stays open a while, as Telegram's does
			f.mutex.Lock()
			var due []string
			for i := max(asked.Offset-1, 0); i < len(f.updates); i++ {
				due = append(due, fmt.Sprintf(`{"update_id":%d,"message":%s}`, i+1, f.updates[i]))
			}
			f.mutex.Unlock()
			if len(due) > 0 {
				fmt.Fprintf(w, `{"ok":true,"result":[%s]}`, strings.Join(due, ","))
				return
			}
			time.Sleep(10 * time.Millisecond)
		}
		fmt.Fprint(w, `{"ok":true,"result":[]}`)
		return
	}
	chat, words := r.FormValue("chat_id"), r.FormValue("caption")
	if r.Header.Get("Content-Type") == "application/json" {
		var body struct {
			ChatID int64 `json:"chat_id"`
			Text   string
		}
		json.NewDecoder(r.Body).Decode(&body)
		chat, words = strconv.FormatInt(body.ChatID, 10), body.Text
	}
	f.calls <- chat + " " + method + ": " + words
	if method == "setMessageReaction" && chat == "43" {
		fmt.Fprint(w, `{"ok":false,"description":"Bad Request: REACTION_INVALID"}`)
		return
	}
	fmt.Fprint(w, `{"ok":true,"result":{"message_id":7}}`)
}

func TestBot(t *testing.T) {
	api := &fakeAPI{calls: make(chan string, 20)}
	server := httptest.NewServer(api)
	defer server.Close()
	bot := telegram.New("token", 42, 43)
	bot.API = server.URL

	answers := make(chan hotline.Message, 5)
	desk := hotline.New(&hotline.Memory{}, []hotline.Operators{bot}, []hotline.Listener{
		hotline.ListenerFunc(func(_ context.Context, _ hotline.Ticket, m hotline.Message) error { answers <- m; return nil }),
	})
	desk.Retry = 50 * time.Millisecond
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go desk.Run(ctx)
	go bot.Listen(ctx, desk)

	expect := func(want ...string) {
		t.Helper()
		api.expect(t, want...)
	}
	answered := func(text string, closed bool) {
		t.Helper()
		select {
		case got := <-answers:
			if got.Text != text || got.Closed != closed {
				t.Fatalf("the user got %+v", got)
			}
		case <-time.After(2 * time.Second):
			t.Fatalf("%q never reached the user", text)
		}
	}
	write := func(user, ticket string, m hotline.Draft) {
		t.Helper()
		if _, err := desk.Write(ctx, user, ticket, m); err != nil {
			t.Fatal(err)
		}
	}

	write("ann", "", hotline.Draft{Text: "it broke", Files: [][]byte{[]byte("\x89PNG\r\n\x1a\n")}})
	api.push(42, "/online", "")
	expect("42 sendMessage: Your shift is open. Tickets that wait come shortly.", "42 sendMessage: #1 · ann\n\nit broke", "42 sendDocument: #1 · ann")

	api.push(43, "/online", "")
	expect("43 sendMessage: Your shift is open. Tickets that wait come shortly.")
	write("bob", "", hotline.Draft{Text: "where is my knife"})
	expect("43 sendMessage: #2 · bob\n\nwhere is my knife") // 43 has fewer open tickets

	api.push(42, "/offline", "")
	expect("42 sendMessage: Your shift is closed. Open tickets of yours: 1. Each goes to whoever is on shift when its user writes again.")
	write("ann", "1", hotline.Draft{Text: "hello?"})
	expect("43 sendMessage: #1 · ann\n\nhello?")

	api.push(43, "hi", "") // not a reply
	expect("43 sendMessage: /online opens your shift and /offline closes it. To answer a ticket, reply to the bot's message about it. /close at the start of a reply closes the ticket.")
	api.push(43, "", "#1 · ann") // a photo, no words
	expect("43 sendMessage: Only words go from Telegram into a ticket.")
	api.push(43, "/closed is no command", "#1 · ann")
	expect("43 setMessageReaction: ", "43 sendMessage: Sent to #1.") // chat 43 takes no reactions
	answered("/closed is no command", false)

	api.push(7, "/close", "#1 · ann") // not an operator's chat: nothing happens
	api.push(43, "/close fixed, thanks for the screenshot", "#1 · ann")
	expect("43 setMessageReaction: ", "43 sendMessage: Sent to #1.")
	answered("fixed, thanks for the screenshot", true)
	api.push(43, "one more thing", "#1 · ann")
	expect("43 sendMessage: #1 is closed: the answer did not go.")
}

func TestShared(t *testing.T) {
	api := &fakeAPI{calls: make(chan string, 20)}
	server := httptest.NewServer(api)
	defer server.Close()
	bot := telegram.New("token", 42)
	bot.API = server.URL
	bot.Texts.ShiftOpen = "Смена открыта."
	bot.About = func(ticket hotline.Ticket) string { return ticket.Fields["topic"] + " · " + ticket.User }
	desk := hotline.New(&hotline.Memory{}, []hotline.Operators{bot}, nil)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go desk.Run(ctx)

	for _, notOurs := range []json.RawMessage{
		written(42, "/start token", ""),             // the app's command
		written(7, "/online", ""),                   // not an operator's chat
		written(42, "thanks", "Your deal is paid."), // a reply to the app's own message
	} {
		if bot.Take(ctx, desk, notOurs) {
			t.Fatalf("the bot took %s, which is the app's", notOurs)
		}
	}
	if !bot.Take(ctx, desk, written(42, "/online", "")) {
		t.Fatal("the bot left /online to the app")
	}
	api.expect(t, "42 sendMessage: Смена открыта.")
	if _, err := desk.Write(ctx, "ann", "", hotline.Draft{Text: "where is my knife", Fields: map[string]string{"topic": "Сделка"}}); err != nil {
		t.Fatal(err)
	}
	api.expect(t, "42 sendMessage: #1 · Сделка · ann\n\nwhere is my knife")
}
