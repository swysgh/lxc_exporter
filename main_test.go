package main

import (
	"flag"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// parseFlags 用给定参数解析出一份配置。
func parseFlags(t *testing.T, args ...string) flags {
	t.Helper()
	var f flags
	fs := flag.NewFlagSet("test", flag.ContinueOnError)
	fs.SetOutput(&strings.Builder{})
	registerFlags(fs, &f)
	if err := fs.Parse(args); err != nil {
		t.Fatalf("解析参数 %v 失败: %v", args, err)
	}
	return f
}

// parseFlagsWithConfig 解析参数并应用 -config 指向的配置文件。
func parseFlagsWithConfig(t *testing.T, args ...string) (flags, error) {
	t.Helper()
	var f flags
	fs := flag.NewFlagSet("test", flag.ContinueOnError)
	fs.SetOutput(&strings.Builder{})
	registerFlags(fs, &f)
	if err := fs.Parse(args); err != nil {
		t.Fatalf("解析参数 %v 失败: %v", args, err)
	}
	if f.configPath != "" {
		cfg, err := loadConfig(f.configPath)
		if err != nil {
			return f, err
		}
		if err := mergeConfig(fs, &f, cfg); err != nil {
			return f, err
		}
	}
	return f, nil
}

func writeConfig(t *testing.T, content string) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "config.json")
	if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
		t.Fatalf("写入临时配置失败: %v", err)
	}
	return path
}

func TestDefaultFlags(t *testing.T) {
	f := parseFlags(t)
	if f.listenAddress != ":9328" {
		t.Errorf("默认监听地址 = %q，期望 :9328", f.listenAddress)
	}
	if f.telemetryPath != "/metrics" {
		t.Errorf("默认指标路径 = %q，期望 /metrics", f.telemetryPath)
	}
	if f.cgroupRoot != "/sys/fs/cgroup" {
		t.Errorf("cgroup-root 默认 = %q", f.cgroupRoot)
	}
	if f.procRoot != "/proc" {
		t.Errorf("proc-root 默认 = %q", f.procRoot)
	}
	if !f.lxcNetwork {
		t.Error("lxc.network 默认应为开启")
	}
	if f.lxcInclude != "" || f.lxcExclude != "" {
		t.Errorf("筛选默认 = %q / %q，期望空", f.lxcInclude, f.lxcExclude)
	}
	if f.commandTimeout != 20*time.Second {
		t.Errorf("command-timeout 默认 = %v，期望 20s", f.commandTimeout)
	}
}

func TestConfigApplied(t *testing.T) {
	path := writeConfig(t, `{
		"listen_address": ":19328",
		"auth_token": "tok",
		"cgroup_root": "/tmp/cg",
		"proc_root": "/tmp/proc",
		"lxc_network": false,
		"lxc_include": "^demo",
		"command_timeout": "5s"
	}`)

	f, err := parseFlagsWithConfig(t, "-config="+path)
	if err != nil {
		t.Fatalf("合并配置失败: %v", err)
	}
	if f.listenAddress != ":19328" {
		t.Errorf("listenAddress = %q", f.listenAddress)
	}
	if f.authToken != "tok" {
		t.Errorf("authToken = %q", f.authToken)
	}
	if f.cgroupRoot != "/tmp/cg" {
		t.Errorf("cgroupRoot = %q", f.cgroupRoot)
	}
	if f.procRoot != "/tmp/proc" {
		t.Errorf("procRoot = %q", f.procRoot)
	}
	if f.lxcNetwork {
		t.Error("配置文件 lxc_network=false 应当生效")
	}
	if f.lxcInclude != "^demo" {
		t.Errorf("lxcInclude = %q", f.lxcInclude)
	}
	if f.commandTimeout != 5*time.Second {
		t.Errorf("commandTimeout = %v", f.commandTimeout)
	}
	// 未出现的项保留默认。
	if f.telemetryPath != "/metrics" {
		t.Errorf("telemetryPath = %q，期望保留默认 /metrics", f.telemetryPath)
	}
}

func TestFlagOverridesConfig(t *testing.T) {
	path := writeConfig(t, `{"listen_address": ":19328", "lxc_network": false}`)
	f, err := parseFlagsWithConfig(t, "-config="+path,
		"-web.listen-address=:12345", "-lxc.network=true")
	if err != nil {
		t.Fatalf("合并配置失败: %v", err)
	}
	if f.listenAddress != ":12345" {
		t.Errorf("listenAddress = %q，期望命令行值", f.listenAddress)
	}
	if !f.lxcNetwork {
		t.Error("命令行 -lxc.network=true 应覆盖配置文件")
	}
}

func TestConfigUnknownKey(t *testing.T) {
	path := writeConfig(t, `{"nope": 1}`)
	if _, err := parseFlagsWithConfig(t, "-config="+path); err == nil {
		t.Error("未知键应当导致解析失败")
	}
}

func TestConfigInvalidDuration(t *testing.T) {
	path := writeConfig(t, `{"command_timeout": "abc"}`)
	if _, err := parseFlagsWithConfig(t, "-config="+path); err == nil {
		t.Error("非法 command_timeout 应当导致解析失败")
	}
}

func TestValidateListenAddress(t *testing.T) {
	valid := []string{":9328", "127.0.0.1:19328", "[::1]:9328"}
	for _, a := range valid {
		if err := validateListenAddress(a); err != nil {
			t.Errorf("validateListenAddress(%q) = %v，期望合法", a, err)
		}
	}
	invalid := []string{"bad", "127.0.0.1", ":0", ":70000", ""}
	for _, a := range invalid {
		if err := validateListenAddress(a); err == nil {
			t.Errorf("validateListenAddress(%q) 期望报错", a)
		}
	}
}

func TestMaskToken(t *testing.T) {
	if got := maskToken(""); got != "(未启用)" {
		t.Errorf("maskToken(\"\") = %q", got)
	}
	if got := maskToken("abcdef123456"); got != "abcd***" {
		t.Errorf("maskToken = %q，期望 abcd***", got)
	}
}

func TestVersionString(t *testing.T) {
	v := versionString()
	if !strings.Contains(v, "lxc_exporter") {
		t.Errorf("版本串 = %q，应当包含 lxc_exporter", v)
	}
}
