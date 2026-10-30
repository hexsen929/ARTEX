package server

import (
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"strconv"
	"strings"

	"github.com/Autumn-27/artex/db"
)

func runtimeControlKeyAllowed(key string) bool {
	return key == settingRoEEnforcement || key == settingFindingVerifyMode || key == settingLLMRecordsRetentionDays
}

func readRuntimeControlValue(pg *db.DB, key string) (any, error) {
	switch key {
	case settingRoEEnforcement:
		return readRoEEnforcementSetting(pg)
	case settingFindingVerifyMode:
		return readFindingVerifyModeSetting(pg)
	case settingLLMRecordsRetentionDays:
		return readLLMRecordsRetentionSetting(pg)
	default:
		return nil, errors.New("未知的运行控制设置")
	}
}

// A single-control view is deliberately separate from the complete Settings
// snapshot: one damaged peer cannot prevent an operator explicitly repairing a
// different setting, and unknown peers are never coerced or re-saved.
func (s *Server) getRuntimeControl(w http.ResponseWriter, r *http.Request) {
	key := r.PathValue("key")
	if !runtimeControlKeyAllowed(key) {
		writeErr(w, http.StatusNotFound, "运行控制设置不存在")
		return
	}
	if s == nil || s.m == nil || s.m.pg == nil {
		writeErr(w, http.StatusInternalServerError, "运行控制设置数据库尚未就绪")
		return
	}
	value, err := readRuntimeControlValue(s.m.pg, key)
	if err != nil {
		writeErr(w, http.StatusInternalServerError, err.Error())
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{key: value})
}

func decodeRuntimeControlValue(w http.ResponseWriter, r *http.Request) (json.RawMessage, bool) {
	decoder := json.NewDecoder(http.MaxBytesReader(w, r.Body, 4096))
	start, err := decoder.Token()
	if err != nil || start != json.Delim('{') {
		writeErr(w, http.StatusBadRequest, "请求必须仅包含一个 value 字段")
		return nil, false
	}
	var value json.RawMessage
	seen := false
	for decoder.More() {
		key, err := decoder.Token()
		if err != nil || key != "value" || seen {
			writeErr(w, http.StatusBadRequest, "请求必须仅包含一个 value 字段")
			return nil, false
		}
		if err := decoder.Decode(&value); err != nil {
			writeErr(w, http.StatusBadRequest, "运行控制设置值无效")
			return nil, false
		}
		seen = true
	}
	end, err := decoder.Token()
	var extra any
	if err != nil || end != json.Delim('}') || !seen || decoder.Decode(&extra) != io.EOF {
		writeErr(w, http.StatusBadRequest, "请求必须仅包含一个 value 字段")
		return nil, false
	}
	return value, true
}

func (s *Server) putRuntimeControl(w http.ResponseWriter, r *http.Request) {
	key := r.PathValue("key")
	if !runtimeControlKeyAllowed(key) {
		writeErr(w, http.StatusNotFound, "运行控制设置不存在")
		return
	}
	raw, ok := decodeRuntimeControlValue(w, r)
	if !ok {
		return
	}
	var stored string
	var desired any
	var err error
	switch key {
	case settingRoEEnforcement:
		var value *string
		value, err = parseRoEEnforcementUpdate(raw)
		if err == nil && value != nil {
			stored, desired = *value, *value
		}
	case settingFindingVerifyMode:
		var value *string
		value, err = parseFindingVerifyModeUpdate(raw)
		if err == nil && value != nil {
			stored, desired = *value, *value
		}
	case settingLLMRecordsRetentionDays:
		var value *int
		value, err = parseLLMRecordsRetentionUpdate(raw)
		if err == nil && value != nil {
			stored, desired = strconv.Itoa(*value), *value
		}
	}
	if err != nil || desired == nil {
		message := "运行控制设置值无效"
		if err != nil {
			message = err.Error()
		}
		writeErr(w, http.StatusBadRequest, message)
		return
	}
	if s == nil || s.m == nil || s.m.pg == nil {
		writeErr(w, http.StatusInternalServerError, "运行控制设置数据库尚未就绪")
		return
	}
	if err := s.m.pg.SetSettingsAtomic(r.Context(), map[string]string{key: stored}); err != nil {
		writeErr(w, http.StatusInternalServerError, "运行控制设置保存结果未确认，请重新读取后核对")
		return
	}
	actual, err := readRuntimeControlValue(s.m.pg, key)
	if err != nil {
		writeErr(w, http.StatusInternalServerError, "运行控制设置保存后读取失败，结果未确认，请重新读取")
		return
	}
	if actual != desired {
		writeErr(w, http.StatusConflict, "运行控制设置在保存期间已变化，结果未确认，请重新读取")
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{key: actual})
}

func readFindingVerifyModeSetting(pg *db.DB) (string, error) {
	if pg == nil {
		return "", errors.New("自动验证设置数据库尚未就绪")
	}
	value, found, err := pg.GetSetting(settingFindingVerifyMode)
	if err != nil {
		return "", errors.New("自动验证设置读取失败，已有配置保持不变")
	}
	if !found {
		return "auto", nil
	}
	if value != "auto" && value != "off" {
		return "", errors.New("finding_verify_mode 设置无效，请显式保存 auto 或 off")
	}
	return value, nil
}

func parseFindingVerifyModeUpdate(raw json.RawMessage) (*string, error) {
	if len(raw) == 0 {
		return nil, nil
	}
	var value string
	if err := json.Unmarshal(raw, &value); err != nil || value != "auto" && value != "off" {
		return nil, errors.New("finding_verify_mode 必须是 auto 或 off")
	}
	return &value, nil
}

func readLLMRecordsRetentionSetting(pg *db.DB) (int, error) {
	if pg == nil {
		return 0, errors.New("LLM 录制保留设置数据库尚未就绪")
	}
	value, found, err := pg.GetSetting(settingLLMRecordsRetentionDays)
	if err != nil {
		return 0, errors.New("LLM 录制保留设置读取失败；跳过清理，保留已有记录")
	}
	if !found {
		return defaultLLMRecordsRetentionDays, nil
	}
	days, err := strconv.ParseInt(strings.TrimSpace(value), 10, 64)
	if err != nil || days < 0 || days > int64(maxLLMRecordsRetentionDays) {
		return 0, errors.New("llm_records_retention_days 设置无效；跳过清理，请显式保存 0 到 106751 的整数")
	}
	return int(days), nil
}

func parseLLMRecordsRetentionUpdate(raw json.RawMessage) (*int, error) {
	if len(raw) == 0 {
		return nil, nil
	}
	var value *int
	if err := json.Unmarshal(raw, &value); err != nil || value == nil || *value < 0 || *value > maxLLMRecordsRetentionDays {
		return nil, errors.New("llm_records_retention_days 必须是 0 到 106751 的整数（0 表示不清理）")
	}
	return value, nil
}
