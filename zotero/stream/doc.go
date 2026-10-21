// Package stream defines the message types of Zotero's streaming WebSocket
// protocol: what clients send, what servers push, topic naming and close
// codes. It has no WebSocket dependency; pair it with any library.
//
// Server side, read a text frame and validate it:
//
//	msg, err := stream.ParseClientMessage(frame)
//	if err != nil {
//		// close with stream.CloseBadRequest
//	}
//	subs, _ := msg.Subscriptions()
//
// and answer with one of the event types, which marshal themselves with the
// right "event" member:
//
//	b, _ := json.Marshal(stream.Connected{Retry: 10000})
package stream
