# hotline

A support desk for your app with no infrastructure of its own: no database, no queue, no UI and no dependencies beyond the Go standard library.

Users write from your app: a message, with files if they need to. Operators answer wherever they already are: Telegram, Slack, email. In between, hotline keeps the rules of a ticket and makes sure every message gets to the other side.

```
 user ── POST ──► Desk ── its operator's adapter ──► operator on shift   (Telegram, Slack, email…)
 user ◄─ user listeners ── Desk ◄── Answer ── operator
                            │
                          Store                                         (Postgres, SQLite, memory…)
```

## How it works

- **One endpoint.** `Desk.Handler` takes a `multipart/form-data` POST. Without `ticket` it opens a ticket, with `ticket` it adds to one. There is nothing to read back over HTTP: answers come to the user through user listeners.
- **Adapters deliver.** A listener is anything with `Deliver(ctx, ticket, message) error`. An operator adapter also says which operators it serves. The desk gives each ticket to an operator on shift and brings what the user writes to that operator's adapter alone. What an operator writes goes to every user listener.
- **Operators answer through `Desk.Answer`.** An operator adapter turns a reply in its channel into a call. The Telegram adapter does it when an operator replies to the bot's message about a ticket.
- **Nothing is lost.** A message is saved before it is delivered. If a listener fails, or nobody can take the message, it goes again after `Retry`. Delivery is at least once: a listener may see a message twice, and `Message.ID` tells the copies apart. Each side of a ticket hears its messages in the order they were written.
- **The rules live in one place.** The desk passes its rules to the `Store` as callbacks inside its atomic operations. The few a store keeps itself are named in the interface, and package `storetest` checks them.

## Rules

- A user writes only into their own tickets. Someone else's ticket answers `ErrNoTicket`, as if it did not exist.
- Either side can close a ticket with its message, with words or without them. Nothing more goes into a closed ticket.
- `Limits` cap a message's characters, its files, the size of each file, how many tickets a user has open and how many messages a user writes in a row before an operator answers. The defaults are 2000 characters, 3 files of 8 MB, 5 open tickets and 5 messages in a row.
- A file's type is sniffed from its bytes, never taken from the sender, and has to be one of `Limits.Types`: PNG, JPEG, PDF and plain text by default. File names are dropped, since a name is one more thing the sender controls. If you serve a file, send it with its `Type` and `X-Content-Type-Options: nosniff`. For example, an SVG without an XML header sniffs as plain text, and that header keeps it plain text in the browser.
- A draft the desk does not take is `ErrBadMessage`, and one of `ErrEmpty`, `ErrNotText`, `ErrLongText`, `ErrManyFiles`, `ErrLargeFile` or `ErrFileType` says why.
- `Draft.Fields` hold what your app knows about a new ticket, such as its topic or an order. They are given when the ticket opens, the store keeps them in `Ticket.Fields`, and adapters may show them. A draft into an open ticket, or an answer, carries none.

## Use

```go
store := &hotline.Memory{} // or your own Store
bot := telegram.New(os.Getenv("BOT_TOKEN"), annaChatID, borisChatID, veraChatID)
inbox := hotline.ListenerFunc(func(ctx context.Context, t hotline.Ticket, m hotline.Message) error {
	return notifyUser(ctx, t.User, t.ID, m.Text) // your app's notifications, email, a webhook…
})

desk := hotline.New(store, []hotline.Operators{bot}, []hotline.Listener{inbox})
go desk.Run(ctx)         // delivers messages
go bot.Listen(ctx, desk) // takes operators' shifts and replies

http.Handle("POST /support", desk.Handler(func(r *http.Request) (string, error) {
	return userFromSession(r) // whoever your sign-in says it is
}))
```

```sh
curl -F text="The payment page is blank" -F file=@screenshot.png https://example.com/support
# 201 {"message":"1","ticket":"1"}
curl -F ticket=1 -F text="Still blank today" https://example.com/support
```

Errors come back as `{"error": code}`:

- 400: `empty`, `not_text`, `too_long`, `too_many_files`, `file_too_large`, `file_type` and `bad_form`;
- 401: `unauthorized`;
- 404: `no_ticket`;
- 409: `closed`, `wait_for_answer` and `too_many_tickets`;
- 413: `too_large`.

## Writing an adapter

An operator adapter is a listener that also says which operators it serves. Return `nil` from `Deliver` once the message is safely there, and an error to have it go again later. The desk gives an adapter only the tickets of its own operators. An operator the adapter stops serving loses their tickets the next time a user writes in one.

```go
type slack struct{ members []string }

func (s slack) Serves(operator string) bool {
	member, ours := strings.CutPrefix(operator, "slack:")
	return ours && slices.Contains(s.members, member)
}

func (s slack) Deliver(ctx context.Context, t hotline.Ticket, m hotline.Message) error {
	return postToSlack(ctx, strings.TrimPrefix(t.Operator, "slack:"), fmt.Sprintf("#%s · %s\n%s", t.ID, t.User, m.Text))
}
```

An adapter of one operator with no shifts of their own, such as a mailbox, is `hotline.Operator("mail:support", mailer)`. That operator is always on shift: `Run` opens the shift at start, and `desk.Shift` does not close it.

An adapter that also takes answers calls `desk.Answer(ctx, ticketID, hotline.Draft{Text: …, Close: …})`. The ticket's ID has to travel with the message so the reply can find it. The Telegram adapter starts every message with `#<id> · ` for that.

## Writing a store

`Store` has six methods:

- `Open` and `Add` save messages and call the desk's rule under the store's lock.
- `Next` and `Delivered` are the outbox. `Next` hides the message it returns for a while instead of locking it, so several processes can run `Desk.Run` over one database and a crashed one loses nothing. It gives each side of a ticket its messages in order. In SQL, `Next` is one `update … where id = (select … for update skip locked) returning …`, and the inner select takes only the first undelivered message of each ticket and side.
- `Shift` keeps who is on shift, and `Assign` gives a ticket the operator the desk's rule picks. The rule sees every operator on shift and how many open tickets each has.

A store proves itself the way `Memory` does:

```go
func TestPostgres(t *testing.T) {
	storetest.Run(t, func() hotline.Store { return newPostgres(t) })
}
```

## Shifts

Operators get tickets only on shift. An adapter opens and closes a shift with `desk.Shift(ctx, operator, on)` when an operator says so. A shift opens only for an operator some adapter serves. The store keeps shifts, so a restart changes nothing. There is no presence and no timer behind a shift: it is open while the operator says so.

- A ticket goes to the served operator on shift with the fewest open tickets. Closed tickets count for nobody.
- A ticket stays with its operator while they are on shift and served. Otherwise it moves the next time its user writes, and the new operator does not see the earlier messages.
- With nobody on shift, messages wait in the queue and go to the first operator who opens a shift, within `Retry`.
- An operator name belongs to its adapter and is unique among adapters, such as `telegram:42`. An operator made with `hotline.Operator`, such as a mailbox, is always on shift.
- A listener can also answer `hotline.ErrNobody` itself. The message waits in the same way, without being logged as a failure.

`Retry`, a minute by default, is how long a taken message is the desk's own to deliver. Listeners get nine tenths of it, so no second process takes the message while the first still works on it.

## Telegram

`telegram.New(token, chats...)` takes the operators' chats. A chat is the operator `telegram:<chat id>`: a person's private chat, or a group that answers as one team. Only these chats take tickets and answer them. `bot.API` points the bot at a Bot API server of your own.

- `/online` opens the operator's shift and `/offline` closes it. On `/offline`, the bot says how many open tickets the operator leaves.
- An operator answers by replying to the bot's message or to one of its files. `/close` at the start of a reply closes the ticket. A thumbs-up on the reply means it went into the ticket, and where Telegram takes no reaction, the bot says so in words. Only words go from Telegram into a ticket for now.
- A chat removed from `Chats` is served no more, so its tickets move on. For operators your app keeps itself, who come and go while it runs, set `bot.IsOperator` instead of `Chats`.
- `bot.Texts` are the bot's words, `telegram.English` to begin with. `bot.About` says what a heading shows after `#12 · `: the user by default, or anything made of `Ticket.Fields`.

A bot your app already uses, for its own commands, cannot be polled twice: Telegram answers a second `getUpdates` with 409. Read the updates in your app, skip `Listen`, and hand each message to `Take`. It answers whether the message was the bot's: a shift command or a reply to a ticket, in an operator's chat. Everything else is your app's, such as `/start`.

```go
for _, update := range updates {
	if !bot.Take(ctx, desk, update.Message) { // update.Message is json.RawMessage
		handleYourself(update)
	}
}
```

Such an app can ask the Bot API for its own work through the same bot: `bot.Call(ctx, method, body, &result)`. A method Telegram did not do is a `*telegram.Refusal` with its error code, such as 403 for a chat that blocked the bot.

## Not here yet

- Reading a ticket back over HTTP, and a list of tickets that wait for an answer: both need a read method on `Store`.
- A Postgres store.
- Files from operators to users. The desk takes them, the Telegram adapter does not send them yet.
- Removing metadata from images, such as a phone's location. Re-encoding PNG and JPEG from their pixels does it with the standard library.

## License

MIT
