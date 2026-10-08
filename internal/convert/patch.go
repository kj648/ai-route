package convert

import (
	"bytes"
	"encoding/json"
	"fmt"
)

// MergeJSON deep-merges the JSON object patch into the JSON object body:
// objects are merged key by key, any other value replaces the existing one
// and null deletes the key. Untouched fields keep their original bytes.
func MergeJSON(body []byte, patch json.RawMessage) ([]byte, error) {
	var dst map[string]json.RawMessage
	if err := json.Unmarshal(body, &dst); err != nil {
		return nil, fmt.Errorf("invalid JSON body: %w", err)
	}
	var src map[string]json.RawMessage
	if err := json.Unmarshal(patch, &src); err != nil {
		return nil, fmt.Errorf("invalid patch: %w", err)
	}
	if err := mergeObjects(dst, src); err != nil {
		return nil, err
	}
	return marshalRaw(dst)
}

// marshalRaw encodes without HTML escaping, so raw values keep their bytes.
func marshalRaw(v any) ([]byte, error) {
	var buf bytes.Buffer
	enc := json.NewEncoder(&buf)
	enc.SetEscapeHTML(false)
	if err := enc.Encode(v); err != nil {
		return nil, err
	}
	return bytes.TrimRight(buf.Bytes(), "\n"), nil
}

func mergeObjects(dst, src map[string]json.RawMessage) error {
	for k, v := range src {
		if isNullOrEmpty(v) {
			delete(dst, k)
			continue
		}
		if isJSONObject(v) && isJSONObject(dst[k]) {
			var d, s map[string]json.RawMessage
			if err := json.Unmarshal(dst[k], &d); err != nil {
				return err
			}
			if err := json.Unmarshal(v, &s); err != nil {
				return err
			}
			if err := mergeObjects(d, s); err != nil {
				return err
			}
			merged, err := marshalRaw(d)
			if err != nil {
				return err
			}
			dst[k] = merged
			continue
		}
		dst[k] = v
	}
	return nil
}

func isJSONObject(raw json.RawMessage) bool {
	t := bytes.TrimLeft(raw, " \t\r\n")
	return len(t) > 0 && t[0] == '{'
}
