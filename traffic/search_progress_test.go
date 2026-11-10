package traffic

import (
	"encoding/json"
	"fmt"
	"testing"
)

func TestTrafficSearchProgressSignalsEndAndStableTies(t *testing.T) {
	tr, _ := openTraffic(t)
	for i := 0; i < 20; i++ {
		tr.record(newFlow("fixture.invalid", "GET", fmt.Sprintf("/saved/%d", i), nil, []byte("fixture body must not appear in index")))
	}
	if _, err := tr.db.Exec(`UPDATE exchanges SET ts=42`); err != nil {
		t.Fatal(err)
	}
	var seen = map[string]bool{}
	for _, tool := range tr.Tools() {
		if tool.Name() != "traffic_search" {
			continue
		}
		for page := 0; page < 3; page++ {
			raw, _ := json.Marshal(map[string]any{"host": "fixture.invalid", "page": page, "limit": 10})
			res, err := tool.Call(t.Context(), raw, nil)
			var result struct {
				Items []struct {
					ID string `json:"id"`
				} `json:"items"`
				Page    int  `json:"page"`
				Limit   int  `json:"limit"`
				HasMore bool `json:"has_more"`
				Next    *int `json:"next_page"`
			}
			if err != nil || res.IsError || json.Unmarshal([]byte(res.Flatten()), &result) != nil {
				t.Fatal(res.Flatten(), err)
			}
			want := 10
			if page == 2 {
				want = 0
			}
			if len(result.Items) != want || result.Page != page || result.Limit != 10 || result.HasMore != (page == 0) {
				t.Fatalf("wrong page boundary: %+v", result)
			}
			if page == 0 && (result.Next == nil || *result.Next != 1) || page > 0 && result.Next != nil {
				t.Fatal("wrong continuation", result.Next)
			}
			for _, item := range result.Items {
				if seen[item.ID] {
					t.Fatal("equal-timestamp page duplicated a record")
				}
				seen[item.ID] = true
			}
		}
		bad, err := tool.Call(t.Context(), json.RawMessage(`{"host":"fixture.invalid","page":"invalid"}`), nil)
		if err != nil || !bad.IsError {
			t.Fatal("invalid pagination silently became first page")
		}
	}
	if len(seen) != 20 {
		t.Fatal("paging lost records", len(seen))
	}
}
