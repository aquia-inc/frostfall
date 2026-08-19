package discover

import "encoding/json"

func unmarshalStrings(raw json.RawMessage, out *[]string) error {
	return json.Unmarshal(raw, out)
}
