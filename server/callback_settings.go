package server

import (
	"errors"
	"net/http"
	"os"
	"strings"

	"github.com/Autumn-27/artex/config"
	"github.com/Autumn-27/artex/tunnel"
)

// callbackAddress is deliberately read on each operation. No mutable process
// cache or agent rebuild is needed, and existing plans retain their own address.
func (s *Server) callbackAddress() (config.CallbackAddress, error) {
	if s == nil || s.m == nil || s.m.pg == nil {
		return config.CallbackAddress{}, errors.New("回连地址设置数据库尚未就绪")
	}
	return s.m.CallbackAddress()
}

// CallbackAddress also serves the startup banner, using the existing database
// connection and the same priority rules as live requests and doctor.
func (m *Manager) CallbackAddress() (config.CallbackAddress, error) {
	if m == nil || m.pg == nil {
		return config.CallbackAddress{}, errors.New("回连地址设置数据库尚未就绪")
	}
	return config.ResolveCallbackAddr(m.pg)
}

func (s *Server) callbackHostForTunnel(adapter string) (string, error) {
	if adapter == tunnel.AdapterSuo5 {
		return "", nil
	}
	return s.callbackHost()
}

func (s *Server) writeSettingsPayload(w http.ResponseWriter) {
	pools, poolErr := s.m.WorkerPoolsChecked()
	if poolErr != nil {
		writeErr(w, http.StatusInternalServerError, "Worker 模型池读取失败；保留已有配置，请先修复设置")
		return
	}
	workers := 0
	for _, pool := range pools {
		workers += pool.Workers
	}
	if len(pools) == 0 {
		var err error
		workers, err = s.m.WorkersChecked()
		if err != nil {
			writeErr(w, http.StatusInternalServerError, "Worker 并发设置读取失败；保留已有配置")
			return
		}
	}
	callback, err := s.callbackAddress()
	if err != nil && callback.Source == "" {
		writeErr(w, http.StatusInternalServerError, "回连地址读取失败："+err.Error())
		return
	}
	payload := s.settingsPayload()
	payload["worker_pools"] = pools
	payload["workers"] = workers
	payload["callback_addr"] = callback.Address
	payload["callback_addr_source"] = callback.Source
	payload["callback_addr_locked"] = callback.Locked
	if err != nil {
		payload["callback_addr_error"] = err.Error()
	}
	writeJSON(w, http.StatusOK, payload)
}

// prepareCallbackUpdate rejects invalid or externally managed values before a
// mixed settings request can mutate any unrelated setting.
func (s *Server) prepareCallbackUpdate(raw *string) (string, int, error) {
	if raw == nil {
		return "", 0, nil
	}
	if strings.TrimSpace(os.Getenv("ARTEX_CALLBACK_ADDR")) != "" {
		return "", http.StatusConflict, errors.New("回连地址由 ARTEX_CALLBACK_ADDR 环境变量管理；移除该环境变量后可在后台设置")
	}
	if s == nil || s.m == nil || s.m.pg == nil {
		return "", http.StatusInternalServerError, errors.New("回连地址设置数据库尚未就绪")
	}
	if _, _, err := s.m.pg.GetSetting(config.CallbackAddrSetting); err != nil {
		return "", http.StatusInternalServerError, errors.New("回连地址设置读取失败")
	}
	value, err := config.ValidateCallbackAddr(*raw)
	if err != nil {
		return "", http.StatusBadRequest, err
	}
	return value, 0, nil
}
