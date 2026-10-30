package server

import (
	"errors"
	"net/url"
	"strconv"

	"github.com/Autumn-27/artex/db"
)

type notifyGlobalSettings struct {
	Enabled       bool
	PublicBaseURL string
	DigestMinutes int
}

// A settings read failure is not an empty address or a default-on switch. Keep
// the notification page and the general Settings DTO on the same known snapshot.
func readNotifyGlobalSettings(pg *db.DB) (notifyGlobalSettings, error) {
	result := notifyGlobalSettings{Enabled: true, DigestMinutes: notifyDefaultDigestMinutes}
	if pg == nil {
		return result, errors.New("通知设置数据库尚未就绪")
	}
	for _, key := range []string{settingNotifyEnabled, settingNotifyPublicBaseURL, settingNotifyDigestMinutes} {
		value, found, err := pg.GetSetting(key)
		if err != nil {
			return result, errors.New("通知全局设置读取失败，已有配置保持不变")
		}
		if !found {
			continue
		}
		switch key {
		case settingNotifyEnabled:
			switch value {
			case "true", "1":
				result.Enabled = true
			case "false", "0":
				result.Enabled = false
			default:
				return result, errors.New("通知启用状态无效，需显式修复设置")
			}
		case settingNotifyPublicBaseURL:
			if value != "" {
				u, err := url.Parse(value)
				if err != nil || u.Hostname() == "" || u.Scheme != "http" && u.Scheme != "https" {
					return result, errors.New("通知回链地址设置无效，已有值未修改")
				}
			}
			result.PublicBaseURL = value
		case settingNotifyDigestMinutes:
			n, err := strconv.Atoi(value)
			if err != nil || n < 1 || n > 1440 {
				return result, errors.New("通知汇总周期设置无效，需显式保存 1 到 1440 分钟")
			}
			result.DigestMinutes = n
		}
	}
	return result, nil
}
