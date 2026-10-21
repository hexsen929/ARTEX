package server

import (
	"context"
	"encoding/json"
	"fmt"
)

// Keep the original list fields intact and store the complete detail separately.
// Neither source text nor inline response bodies enter the Agent asset context.
func ssWithDetail(ctx context.Context, cl ssToolCaller, raw json.RawMessage) json.RawMessage {
	var fields map[string]json.RawMessage
	if json.Unmarshal(raw, &fields) != nil || fields == nil {
		return raw
	}
	id := ssSourceAssetID(raw)
	var detail json.RawMessage
	var err error
	if id == "" {
		err = fmt.Errorf("源记录缺少资产 ID，无法获取详情")
	} else {
		var text string
		text, err = cl.Call(ctx, "get_asset_detail", map[string]any{"asset_type": "asset", "id": id})
		if err == nil {
			var object map[string]json.RawMessage
			if json.Unmarshal([]byte(text), &object) != nil || len(object) == 0 {
				err = fmt.Errorf("资产详情不是有效对象")
			} else {
				detail = json.RawMessage(text)
			}
		}
	}
	if err != nil {
		fields["_sync_detail_error"], _ = json.Marshal(err.Error())
	} else {
		fields["_scopesentry_detail"] = detail
	}
	merged, err := json.Marshal(fields)
	if err != nil {
		return raw
	}
	return merged
}

func ssRawWarnings(raw json.RawMessage) string {
	var fields struct {
		DetailError string `json:"_sync_detail_error"`
	}
	_ = json.Unmarshal(raw, &fields)
	return fields.DetailError
}
