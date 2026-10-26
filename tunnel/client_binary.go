package tunnel

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"strings"

	"github.com/Autumn-27/artex/session"
	"github.com/Autumn-27/artex/tools"
)

type chiselClientBinary struct {
	path     string
	platform string
	sha256   string
}

// prepareChisel runs before creating a tunnel row or terminating an old client.
// The server belongs to this host; the client belongs to the session's machine.
// Both the initial deployment and every redeploy take this same path.
func (m *Manager) prepareChisel(ctx context.Context, sess session.Session, p *Plan) (chiselClientBinary, error) {
	if err := validateChiselBinary(m.chiselPath, runtime.GOOS+"/"+runtime.GOARCH, true); err != nil {
		return chiselClientBinary{}, fmt.Errorf("本机 chisel 预检失败: %w", err)
	}
	platform, err := detectChiselTarget(ctx, sess)
	if err != nil {
		return chiselClientBinary{}, err
	}
	client, err := m.resolveChiselClient(platform)
	if err != nil {
		return chiselClientBinary{}, err
	}
	p.ClientPlatform, p.ClientSHA256 = client.platform, client.sha256
	return client, nil
}

func validateChiselBinary(path, platform string, executable bool) error {
	if path == "" {
		return fmt.Errorf("未配置 %s 的 chisel 文件", platform)
	}
	st, err := os.Stat(path)
	if err != nil {
		return fmt.Errorf("读取 %s: %w", path, err)
	}
	if !st.Mode().IsRegular() {
		return fmt.Errorf("chisel 路径不是普通文件: %s", path)
	}
	actual := tools.InspectExecutablePlatform(path)
	if actual == "" {
		return fmt.Errorf("chisel 文件不是可识别的原生二进制: %s", path)
	}
	if !tools.PlatformMatches(actual, platform) {
		return fmt.Errorf("chisel 文件平台为 %s，需要 %s: %s", actual, platform, path)
	}
	if executable && runtime.GOOS != "windows" && st.Mode().Perm()&0o111 == 0 {
		return fmt.Errorf("本机 chisel 缺少执行权限: %s", path)
	}
	return nil
}

func (m *Manager) resolveChiselClient(platform string) (chiselClientBinary, error) {
	// Remote process control currently uses setsid and /proc PID ownership checks.
	// Having a Windows/Darwin resource on disk does not implement that lifecycle.
	if !strings.HasPrefix(platform, "linux/") {
		return chiselClientBinary{}, fmt.Errorf("目标平台 %s 的自动 chisel 拉起与回收尚未实现；当前自动部署支持 Linux 目标", platform)
	}
	parts := strings.Split(platform, "/")
	if len(parts) != 2 || !supportedChiselArch(parts[1]) {
		return chiselClientBinary{}, fmt.Errorf("未支持的 chisel 目标平台: %s", platform)
	}
	path := m.chiselTargetPath
	if path == "" {
		path = filepath.Join(m.chiselTargetsDir, parts[0]+"-"+parts[1], "chisel")
	}
	if err := validateChiselBinary(path, platform, false); err != nil {
		return chiselClientBinary{}, fmt.Errorf("目标侧 chisel 预检失败: %w；请放到 tools/targets/%s-%s/chisel，或显式设置 ARTEX_CHISEL_TARGET_PATH（不会使用本机文件替代）", err, parts[0], parts[1])
	}
	sum, err := fileSHA256(path)
	if err != nil {
		return chiselClientBinary{}, fmt.Errorf("目标侧 chisel 摘要读取失败: %w", err)
	}
	return chiselClientBinary{path: path, platform: platform, sha256: sum}, nil
}

func supportedChiselArch(arch string) bool {
	switch arch {
	case "amd64", "arm64", "386", "arm":
		return true
	}
	return false
}

func detectChiselTarget(ctx context.Context, sess session.Session) (string, error) {
	if strings.Contains(strings.ToLower(sess.Kind()), "aspx") {
		return "", fmt.Errorf("Windows 会话的自动 chisel 拉起与回收尚未实现；请选用受支持的会话/隧道方式")
	}
	tag, err := randTag()
	if err != nil {
		return "", fmt.Errorf("目标平台探测标记生成失败: %w", err)
	}
	marker := "ARTEX_CHISEL_" + tag
	command := fmt.Sprintf("printf '\\n%s_OS='; uname -s; printf '%s_ARCH='; uname -m", marker, marker)
	stdout, _, err := sess.Exec(ctx, command, probeTimeout)
	if err != nil {
		return "", fmt.Errorf("chisel 目标平台探测失败: %w", err)
	}
	platform, err := parseChiselTarget(stdout, marker)
	if err != nil {
		return "", err
	}
	if !strings.HasPrefix(platform, "linux/") {
		return "", fmt.Errorf("目标平台 %s 的自动 chisel 拉起与回收尚未实现；当前自动部署支持 Linux 目标", platform)
	}
	return platform, nil
}

func parseChiselTarget(stdout, marker string) (string, error) {
	values := map[string]string{}
	for _, line := range strings.Split(stdout, "\n") {
		line = strings.TrimSpace(line)
		for _, key := range []string{"OS", "ARCH"} {
			prefix := marker + "_" + key + "="
			if strings.HasPrefix(line, prefix) {
				if _, exists := values[key]; exists {
					return "", fmt.Errorf("chisel 目标平台探测返回重复标记，已停止部署")
				}
				values[key] = strings.TrimSpace(strings.TrimPrefix(line, prefix))
			}
		}
	}
	var goos, arch string
	switch strings.ToLower(values["OS"]) {
	case "linux":
		goos = "linux"
	case "darwin":
		goos = "darwin"
	default:
		return "", fmt.Errorf("chisel 目标系统探测结果缺失或不受支持，已停止部署")
	}
	switch strings.ToLower(values["ARCH"]) {
	case "x86_64", "amd64":
		arch = "amd64"
	case "aarch64", "arm64":
		arch = "arm64"
	case "i386", "i486", "i586", "i686", "x86":
		arch = "386"
	case "armv7l", "armv6l", "arm":
		arch = "arm"
	default:
		return "", fmt.Errorf("chisel 目标 CPU 架构探测结果缺失或不受支持，已停止部署")
	}
	return goos + "/" + arch, nil
}
