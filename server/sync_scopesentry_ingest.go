package server

import (
	"encoding/json"
	"fmt"
	"strconv"
	"strings"

	"github.com/Autumn-27/artex/db"
)

type ssPreparedItem struct {
	apply      func(*db.AssetStore, *db.SyncObservation) error
	skipReason string
}

// ssPrepareItem separates source validation from the short database transaction.
// Internal AssetStore derivations keep their existing behavior; the returned
// counters cover source records and this adapter's explicit IP writes only.
func ssPrepareItem(assetType string, raw json.RawMessage) (ssPreparedItem, error) {
	switch assetType {
	case "subdomain":
		var it struct {
			Host  string   `json:"host"`
			Type  string   `json:"type"`
			Value []string `json:"value"`
			IP    []string `json:"ip"`
		}
		if err := json.Unmarshal(raw, &it); err != nil {
			return ssPreparedItem{}, fmt.Errorf("subdomain 解析失败: %w", err)
		}
		if strings.TrimSpace(it.Host) == "" {
			return ssPreparedItem{skipReason: "subdomain 缺少 host，未入库"}, nil
		}
		return ssPreparedItem{apply: func(as *db.AssetStore, item *db.SyncObservation) error {
			id, err := as.UpsertSubdomain(db.UpsertSubdomainReq{Domain: it.Host, RecordType: it.Type, RecordValue: it.Value})
			if err != nil {
				return fmt.Errorf("subdomain %s: %w", it.Host, err)
			}
			item.AssetID = &id
			for _, ip := range it.IP {
				if strings.TrimSpace(ip) == "" {
					continue
				}
				if _, err := as.UpsertIP(db.UpsertIPReq{IP: ip, BoundDomains: []string{it.Host}}); err != nil {
					item.DerivedIPFailed++
					return fmt.Errorf("subdomain %s 关联 IP %s 入库失败，本条已回滚: %w", it.Host, ip, err)
				}
				item.DerivedIPWritten++
			}
			return nil
		}}, nil
	case "app":
		var it struct {
			Name        string `json:"name"`
			Category    string `json:"category"`
			Description string `json:"description"`
			ICP         string `json:"icp"`
			BundleID    string `json:"bundle_id"`
		}
		if err := json.Unmarshal(raw, &it); err != nil {
			return ssPreparedItem{}, fmt.Errorf("app 解析失败: %w", err)
		}
		if strings.TrimSpace(it.Name) == "" {
			return ssPreparedItem{skipReason: "app 缺少 name，未入库"}, nil
		}
		return ssPreparedItem{apply: func(as *db.AssetStore, item *db.SyncObservation) error {
			id, err := as.UpsertApp(db.UpsertAppReq{Name: it.Name, BundleID: it.BundleID, Category: it.Category, Description: it.Description, ICP: it.ICP})
			if err != nil {
				return fmt.Errorf("app %s: %w", it.Name, err)
			}
			item.AssetID = &id
			return nil
		}}, nil
	case "service":
		var it struct {
			Domain        string          `json:"domain"`
			IP            string          `json:"ip"`
			Port          json.RawMessage `json:"port"`
			Service       string          `json:"service"`
			URL           string          `json:"url"`
			Title         string          `json:"title"`
			Status        *int            `json:"status"`
			Products      []string        `json:"products"`
			Icon          string          `json:"icon"`
			Host          string          `json:"host"`
			StatusCode    *int            `json:"statuscode"`
			Technologies  []string        `json:"technologies"`
			FaviconMMH3   json.RawMessage `json:"faviconmmh3"`
			ContentLength *int64          `json:"contentlength"`
		}
		if err := json.Unmarshal(raw, &it); err != nil {
			return ssPreparedItem{}, fmt.Errorf("service 解析失败: %w", err)
		}
		if it.Domain == "" {
			it.Domain = it.Host
		}
		if it.StatusCode != nil {
			it.Status = it.StatusCode
		}
		if len(it.Technologies) > 0 {
			it.Products = it.Technologies
		}
		if len(it.FaviconMMH3) > 0 && string(it.FaviconMMH3) != "null" {
			if json.Unmarshal(it.FaviconMMH3, &it.Icon) != nil {
				it.Icon = string(it.FaviconMMH3)
			}
		}
		if it.Service == "http" || it.Service == "https" || it.URL != "" {
			if strings.TrimSpace(it.URL) == "" {
				return ssPreparedItem{skipReason: "HTTP service 缺少 url，未入库"}, nil
			}
			return ssPreparedItem{apply: func(as *db.AssetStore, item *db.SyncObservation) error {
				id, err := as.UpsertHTTPService(db.UpsertHTTPServiceReq{
					URL: it.URL, Technologies: it.Products, StatusCode: it.Status,
					PageTitle: it.Title, FaviconMMH3: it.Icon, IP: it.IP,
					ContentLength: it.ContentLength,
				})
				if err != nil {
					return fmt.Errorf("service %s: %w", it.URL, err)
				}
				item.AssetID = &id
				return nil
			}}, nil
		}
		if strings.TrimSpace(it.Domain) == "" && strings.TrimSpace(it.IP) == "" {
			return ssPreparedItem{skipReason: "service 缺少 domain 和 ip，未入库"}, nil
		}
		port, err := ssPort(it.Port)
		if err != nil {
			return ssPreparedItem{}, err
		}
		return ssPreparedItem{apply: func(as *db.AssetStore, item *db.SyncObservation) error {
			id, err := as.UpsertOtherService(db.UpsertOtherServiceReq{
				Domain: it.Domain, IP: it.IP, Port: port, ServiceName: it.Service,
			})
			if err != nil {
				return fmt.Errorf("service %s:%d: %w", it.IP, port, err)
			}
			item.AssetID = &id
			return nil
		}}, nil
	default:
		return ssPreparedItem{}, fmt.Errorf("未知资产类型: %s", assetType)
	}
}

func ssPort(raw json.RawMessage) (int, error) {
	text := string(raw)
	if len(raw) > 0 && raw[0] == '"' {
		if err := json.Unmarshal(raw, &text); err != nil {
			return 0, fmt.Errorf("service port 解析失败: %w", err)
		}
	}
	port, err := strconv.Atoi(text)
	if err != nil || port < 1 || port > 65535 {
		return 0, fmt.Errorf("service port 必须是 1 至 65535 的整数，收到 %q", text)
	}
	return port, nil
}

func ssSourceAssetID(raw json.RawMessage) string {
	var fields map[string]json.RawMessage
	if json.Unmarshal(raw, &fields) != nil {
		return ""
	}
	for _, key := range []string{"id", "_id"} {
		value := fields[key]
		var id string
		if json.Unmarshal(value, &id) == nil && id != "" {
			return id
		}
		var object struct {
			OID string `json:"$oid"`
		}
		if json.Unmarshal(value, &object) == nil && object.OID != "" {
			return object.OID
		}
	}
	return ""
}
