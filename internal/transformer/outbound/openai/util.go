package openai

import "encoding/json"

func mergeExtraBodyIntoJSON(body []byte, extra json.RawMessage) ([]byte, error) {
	if len(extra) == 0 {
		return body, nil
	}
	var baseObj map[string]any
	if err := json.Unmarshal(body, &baseObj); err != nil {
		return nil, err
	}
	var extraObj map[string]any
	if err := json.Unmarshal(extra, &extraObj); err != nil {
		return nil, err
	}
	if len(extraObj) == 0 {
		return body, nil
	}
	for k, v := range extraObj {
		baseObj[k] = v
	}
	return json.Marshal(baseObj)
}
