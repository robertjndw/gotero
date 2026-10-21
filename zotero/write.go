package zotero

import "encoding/json"

// WriteFailure describes one object that a multi-object write rejected.
type WriteFailure struct {
	Key     string          `json:"key,omitempty"`
	Code    int             `json:"code"`
	Message string          `json:"message"`
	Data    json.RawMessage `json:"data,omitempty"`
}

// WriteResponse is the body of a successful POST to a collection, search,
// item or fulltext endpoint. Maps are keyed by the index of the object in the
// request array, written as a decimal string.
//
// Clients treat a missing member as a protocol error, so all four always
// appear in the output, as {} when empty.
type WriteResponse struct {
	// Successful holds the full saved object for each created or changed entry.
	Successful map[string]json.RawMessage `json:"successful"`
	// Success maps an index to the saved object's key.
	Success map[string]string `json:"success"`
	// Unchanged maps an index to the key of an object whose content did not change.
	Unchanged map[string]string `json:"unchanged"`
	// Failed holds per-object errors.
	Failed map[string]WriteFailure `json:"failed"`
}

// NewWriteResponse returns a response with all four maps allocated.
func NewWriteResponse() *WriteResponse {
	return &WriteResponse{
		Successful: map[string]json.RawMessage{},
		Success:    map[string]string{},
		Unchanged:  map[string]string{},
		Failed:     map[string]WriteFailure{},
	}
}

// MarshalJSON writes nil maps as empty objects.
func (r WriteResponse) MarshalJSON() ([]byte, error) {
	if r.Successful == nil {
		r.Successful = map[string]json.RawMessage{}
	}
	if r.Success == nil {
		r.Success = map[string]string{}
	}
	if r.Unchanged == nil {
		r.Unchanged = map[string]string{}
	}
	if r.Failed == nil {
		r.Failed = map[string]WriteFailure{}
	}
	type plain WriteResponse // drops the method set, avoiding recursion
	return json.Marshal(plain(r))
}

// DeletedResponse is the body of GET deleted. Every array is always present.
type DeletedResponse struct {
	Collections []string `json:"collections"`
	Searches    []string `json:"searches"`
	Items       []string `json:"items"`
	Tags        []string `json:"tags"`
	Settings    []string `json:"settings"`
}

// MarshalJSON writes nil slices as [].
func (d DeletedResponse) MarshalJSON() ([]byte, error) {
	for _, p := range []*[]string{&d.Collections, &d.Searches, &d.Items, &d.Tags, &d.Settings} {
		if *p == nil {
			*p = []string{}
		}
	}
	type plain DeletedResponse
	return json.Marshal(plain(d))
}
