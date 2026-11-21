// Package telegram is a hotline operator adapter: users' messages go to operators' Telegram chats, and an operator
// answers a ticket by replying to the bot's message about it. "/close" at the start of a reply closes the ticket, and the
// words after it go as the last answer.
//
// An operator takes tickets only on shift, from /online to /offline: the bot tells the desk, which keeps the shift in
// its store and gives each ticket to an operator. Each chat is the operator "telegram:<chat id>". The bot reads by long
// polling, so it needs no public address. A bot the host app also uses for its own work is not polled twice: the app
// reads the updates and hands each message to Take.
package telegram

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"mime"
	"mime/multipart"
	"net/http"
	"net/url"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"time"
	"unicode"

	"github.com/Vebat/hotline"
)

// Texts are what the bot says to operators. Those with a verb get the ticket (%s) or a number (%d).
type Texts struct {
	Help        string // how shifts and answers go
	ShiftOpen   string
	ShiftClosed string
	ShiftLeaves string // %d: the open tickets an operator leaves
	ShiftFailed string
	Sent        string // %s: where a thumbs-up cannot go
	OnlyWords   string
	TooLong     string // %d: Limits.Text
	NoTicket    string // %s
	Closed      string // %s
	NotWritten  string // %s
	ClosedMark  string // ends the heading of a message that closes its ticket
}

// English is what a new bot says.
var English = Texts{
	Help:        "/online opens your shift and /offline closes it. To answer a ticket, reply to the bot's message about it. /close at the start of a reply closes the ticket.",
	ShiftOpen:   "Your shift is open. Tickets that wait come shortly.",
	ShiftClosed: "Your shift is closed.",
	ShiftLeaves: "Your shift is closed. Open tickets of yours: %d. Each goes to whoever is on shift when its user writes again.",
	ShiftFailed: "Your shift is as it was, try again in a minute.",
	Sent:        "Sent to #%s.",
	OnlyWords:   "Only words go from Telegram into a ticket.",
	TooLong:     "A ticket takes up to %d characters at once.",
	NoTicket:    "There is no ticket #%s.",
	Closed:      "#%s is closed: the answer did not go.",
	NotWritten:  "#%s: the answer is not written, try again in a minute.",
	ClosedMark:  " · closed",
}

// head finds the ticket a bot's message is about: it starts "#12 · ".
var head = regexp.MustCompile(`^#(\S+) · `)

// extensions name the files an operator downloads, by the types hotline takes by default. Other types get theirs from
// package mime.
var extensions = map[string]string{"image/png": ".png", "image/jpeg": ".jpg", "application/pdf": ".pdf", "text/plain; charset=utf-8": ".txt"}

func extension(contentType string) string {
	if known, ok := extensions[contentType]; ok {
		return known
	}
	if found, _ := mime.ExtensionsByType(contentType); len(found) > 0 {
		return found[0]
	}
	return ""
}

// Bot is one Telegram bot.
// ponytail: one process reads a bot's updates, Telegram answers a second with 409; a webhook if Listen must scale out.
type Bot struct {
	// Chats are the operators' chats: private ones, or a group that is one operator for a whole team. Only these chats
	// take tickets and answer them.
	Chats []int64
	// API is where the Bot API is: Telegram's, or a Bot API server of your own.
	API string
	// Texts are what the bot says, English to begin with.
	Texts Texts
	// About is what a heading says of a ticket after its number: the user, unless it says otherwise.
	About func(hotline.Ticket) string

	token  string // never goes into an error or the log
	client *http.Client
}

func New(token string, chats ...int64) *Bot {
	// The client waits past the 50 seconds Telegram holds an empty poll open.
	return &Bot{Chats: chats, API: "https://api.telegram.org", Texts: English, token: token, client: &http.Client{Timeout: 70 * time.Second}}
}

// address is where a method of the Bot API is asked.
func (b *Bot) address(method string) string { return b.API + "/bot" + b.token + "/" + method }

// prefix starts the name of each of the bot's operators.
const prefix = "telegram:"

// Serves says the operator is one of the bot's chats. A chat gone from Chats is served no more, so its tickets move on.
func (b *Bot) Serves(operator string) bool {
	_, ok := b.chatOf(operator)
	return ok
}

// chatOf is the chat of one of the bot's operators.
func (b *Bot) chatOf(operator string) (int64, bool) {
	id, ours := strings.CutPrefix(operator, prefix)
	chat, err := strconv.ParseInt(id, 10, 64)
	return chat, ours && err == nil && slices.Contains(b.Chats, chat)
}

// Deliver brings a user's message to the chat of the ticket's operator. The desk gives the bot only its own operators.
func (b *Bot) Deliver(ctx context.Context, t hotline.Ticket, m hotline.Message) error {
	chat, ok := b.chatOf(t.Operator)
	if !ok {
		return fmt.Errorf("telegram: %s is not one of the bot's chats", t.Operator)
	}
	return b.deliverTo(ctx, chat, t, m)
}

// deliverTo brings a user's message to one chat. Files follow the words as documents in reply to them, so a screenshot
// keeps its small print, and each carries the ticket's number: a reply to a file answers the ticket too.
func (b *Bot) deliverTo(ctx context.Context, chat int64, t hotline.Ticket, m hotline.Message) error {
	about := t.User
	if b.About != nil {
		about = b.About(t)
	}
	heading := "#" + t.ID + " · " + about
	if m.Closed {
		heading += b.Texts.ClosedMark
	}
	text := heading
	if m.Text != "" {
		text += "\n\n" + m.Text
	}
	var sent struct {
		MessageID int64 `json:"message_id"`
	}
	if err := b.call(ctx, "sendMessage", map[string]any{"chat_id": chat, "text": text}, &sent); err != nil {
		return err
	}
	for i, file := range m.Files {
		fields := map[string]string{
			"chat_id": strconv.FormatInt(chat, 10), "caption": heading,
			"reply_parameters": fmt.Sprintf(`{"message_id":%d}`, sent.MessageID),
		}
		if err := b.upload(ctx, fields, fmt.Sprintf("file-%d%s", i+1, extension(file.Type)), file.Data); err != nil {
			return err
		}
	}
	return nil
}

// Listen takes operators' commands and replies until ctx ends, and passes them to desk.
func (b *Bot) Listen(ctx context.Context, desk *hotline.Desk) {
	var offset int64
	for ctx.Err() == nil {
		var updates []struct {
			UpdateID int64    `json:"update_id"`
			Message  *message `json:"message"`
		}
		err := b.call(ctx, "getUpdates", map[string]any{"offset": offset, "timeout": 50, "allowed_updates": []string{"message"}}, &updates)
		if err != nil {
			if ctx.Err() == nil {
				slog.Warn("telegram: updates not read", "error", err)
				select {
				case <-time.After(5 * time.Second):
				case <-ctx.Done():
				}
			}
			continue
		}
		for _, update := range updates {
			offset = update.UpdateID + 1
			if said := update.Message; said != nil && !b.take(ctx, desk, *said) && slices.Contains(b.Chats, said.Chat.ID) {
				b.say(ctx, said.Chat.ID, b.Texts.Help)
			}
		}
	}
}

type message struct {
	MessageID int64    `json:"message_id"`
	Text      string   `json:"text"`
	Caption   string   `json:"caption"`
	Chat      chat     `json:"chat"`
	From      *user    `json:"from"`
	ReplyTo   *message `json:"reply_to_message"`
}

type chat struct {
	ID int64 `json:"id"`
}

type user struct {
	IsBot bool `json:"is_bot"`
}

// Take handles one message of the bot's updates, the "message" of an update, for an app that reads the updates itself
// instead of running Listen. It answers whether the message was the bot's: a shift command, or a reply to a ticket, in
// an operator's chat. Anything else, as /start, is the app's to handle.
func (b *Bot) Take(ctx context.Context, desk *hotline.Desk, raw json.RawMessage) bool {
	var said message
	if json.Unmarshal(raw, &said) != nil {
		return false
	}
	return b.take(ctx, desk, said)
}

// take is what an operator wrote, if it is the bot's: a shift command, or a reply that goes into its ticket and gets a
// thumbs-up. When the reply does not go, the bot says why.
func (b *Bot) take(ctx context.Context, desk *hotline.Desk, said message) bool {
	if !slices.Contains(b.Chats, said.Chat.ID) {
		return false
	}
	switch strings.TrimSpace(said.Text) {
	case "/online":
		b.shift(ctx, desk, said.Chat.ID, true)
		return true
	case "/offline":
		b.shift(ctx, desk, said.Chat.ID, false)
		return true
	}
	if said.ReplyTo == nil || said.ReplyTo.From == nil || !said.ReplyTo.From.IsBot {
		return false
	}
	found := head.FindStringSubmatch(said.ReplyTo.Text + said.ReplyTo.Caption)
	if found == nil {
		return false
	}
	answer := hotline.Draft{Text: strings.TrimSpace(said.Text)}
	if rest, closing := strings.CutPrefix(answer.Text, "/close"); closing && (rest == "" || unicode.IsSpace(rune(rest[0]))) {
		answer.Text, answer.Close = strings.TrimSpace(rest), true
	}
	_, err := desk.Answer(ctx, found[1], answer)
	switch {
	case err == nil:
		b.react(ctx, said.Chat.ID, said.MessageID, found[1])
	case errors.Is(err, hotline.ErrEmpty):
		b.say(ctx, said.Chat.ID, b.Texts.OnlyWords)
	case errors.Is(err, hotline.ErrLongText):
		b.say(ctx, said.Chat.ID, fmt.Sprintf(b.Texts.TooLong, desk.Limits.Text))
	case errors.Is(err, hotline.ErrNoTicket):
		b.say(ctx, said.Chat.ID, fmt.Sprintf(b.Texts.NoTicket, found[1]))
	case errors.Is(err, hotline.ErrClosed):
		b.say(ctx, said.Chat.ID, fmt.Sprintf(b.Texts.Closed, found[1]))
	default:
		slog.Error("telegram: answer not written", "ticket", found[1], "error", err)
		b.say(ctx, said.Chat.ID, fmt.Sprintf(b.Texts.NotWritten, found[1]))
	}
	return true
}

// shift opens or closes an operator's shift in the desk, whose rules are in hotline.Desk.Shift.
func (b *Bot) shift(ctx context.Context, desk *hotline.Desk, chatID int64, on bool) {
	open, err := desk.Shift(ctx, prefix+strconv.FormatInt(chatID, 10), on)
	switch {
	case err != nil:
		slog.Error("telegram: shift not changed", "chat", chatID, "error", err)
		b.say(ctx, chatID, b.Texts.ShiftFailed)
	case on:
		b.say(ctx, chatID, b.Texts.ShiftOpen)
	case open > 0:
		b.say(ctx, chatID, fmt.Sprintf(b.Texts.ShiftLeaves, open))
	default:
		b.say(ctx, chatID, b.Texts.ShiftClosed)
	}
}

// react puts a thumbs-up on a reply that went into its ticket. Where Telegram takes no reaction, words say it.
func (b *Bot) react(ctx context.Context, chatID, messageID int64, ticket string) {
	err := b.call(ctx, "setMessageReaction", map[string]any{
		"chat_id": chatID, "message_id": messageID, "reaction": []map[string]string{{"type": "emoji", "emoji": "👍"}},
	}, nil)
	if err != nil {
		b.say(ctx, chatID, fmt.Sprintf(b.Texts.Sent, ticket))
	}
}

func (b *Bot) say(ctx context.Context, chatID int64, text string) {
	if err := b.call(ctx, "sendMessage", map[string]any{"chat_id": chatID, "text": text}, nil); err != nil {
		slog.Warn("telegram: bot did not answer", "error", err)
	}
}

// call asks the Bot API one method.
func (b *Bot) call(ctx context.Context, method string, body, result any) error {
	encoded, err := json.Marshal(body)
	if err != nil {
		return fmt.Errorf("encode %s: %w", method, err)
	}
	request, err := http.NewRequestWithContext(ctx, http.MethodPost, b.address(method), bytes.NewReader(encoded))
	if err != nil {
		return fmt.Errorf("make %s request", method)
	}
	request.Header.Set("Content-Type", "application/json")
	return b.do(request, method, result)
}

// upload sends one file as a document, as a form.
func (b *Bot) upload(ctx context.Context, fields map[string]string, name string, data []byte) error {
	var form bytes.Buffer
	writer := multipart.NewWriter(&form)
	for key, value := range fields {
		if err := writer.WriteField(key, value); err != nil {
			return fmt.Errorf("make sendDocument form: %w", err)
		}
	}
	part, err := writer.CreateFormFile("document", name)
	if err == nil {
		_, err = part.Write(data)
	}
	if err == nil {
		err = writer.Close()
	}
	if err != nil {
		return fmt.Errorf("make sendDocument form: %w", err)
	}
	request, err := http.NewRequestWithContext(ctx, http.MethodPost, b.address("sendDocument"), &form)
	if err != nil {
		return errors.New("make sendDocument request")
	}
	request.Header.Set("Content-Type", writer.FormDataContentType())
	return b.do(request, "sendDocument", nil)
}

// do sends a request and reads the Bot API's answer. The address, with the token in it, stays out of the error.
func (b *Bot) do(request *http.Request, method string, result any) error {
	response, err := b.client.Do(request)
	if err != nil {
		var failure *url.Error
		if errors.As(err, &failure) {
			err = failure.Err
		}
		return fmt.Errorf("reach telegram: %w", err)
	}
	defer response.Body.Close()
	var answer struct {
		OK          bool            `json:"ok"`
		Result      json.RawMessage `json:"result"`
		Description string          `json:"description"`
	}
	if err := json.NewDecoder(io.LimitReader(response.Body, 1<<20)).Decode(&answer); err != nil {
		return fmt.Errorf("read telegram's answer to %s (%d): %w", method, response.StatusCode, err)
	}
	if !answer.OK {
		return fmt.Errorf("telegram refused %s (%d): %s", method, response.StatusCode, answer.Description)
	}
	if result == nil {
		return nil
	}
	return json.Unmarshal(answer.Result, result)
}
