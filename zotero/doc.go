// Package zotero contains the building blocks for talking about Zotero's Web
// API v3 data model: object keys, an order-preserving JSON object, POST patch
// merging, response envelopes and a serializer that writes item JSON in the
// field order Zotero clients expect.
//
// # Layout
//
// This package imports gotero/zotero/schema (item types, field order, schema
// versions) and never the other way around, so the schema tables can be used
// on their own. MarshalItemData lives here and takes a *schema.Schema. The
// WebSocket protocol types are in gotero/zotero/stream.
//
// # Example
//
// Apply a client's POST patch to a stored item and render it for a client
// that has schema version 33:
//
//	var stored, patch zotero.OrderedMap
//	_ = json.Unmarshal(storedJSON, &stored)
//	_ = json.Unmarshal(patchJSON, &patch)
//
//	merged, changed := zotero.Merge(stored, patch)
//	if !changed {
//		// report the object under "unchanged" and keep its version
//	}
//	merged.Set("version", json.RawMessage("42"))
//	out, err := zotero.MarshalItemData(merged, schema.Default(), 33)
//
// # Stability
//
// The module is v0. Names and signatures may change before v1.
package zotero
