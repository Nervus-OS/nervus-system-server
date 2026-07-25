// Package adminclient 是 nervud 管理通道（nervud-admin.sock）的客户端。
//
// # 为什么在这里重写一份而不是 import nervud/internal/adminwire
//
// 那是 internal 包，跨模块 import 不了。而且 adminwire 的设计目标之一就是
// 「叶子包，只依赖标准库」——它的线格式极简（4 字节大端长度 + JSON），
// 照抄一份的成本远低于把整个内核模块拖进本仓库的依赖树。
//
// 代价是两份定义有漂移风险。缓解手段：wire_test.go 里的 golden 断言固定了
// 字节布局与全部 Cmd/Code 常量，任一侧改了都会失败。
//
// # 谁能用它
//
// 只有 nervus.pkgmanagerd。内核的 admin.Config.ServiceUID 只放行这一个包的
// UID，其它进程连 connect() 都过不了（socket 是 0660，组是 pkgmanagerd 的 GID）。
//
// # 它不做安全判定
//
// 本包只负责把命令投递过去、把结果读回来。签名验证、digest 复核、升级裁决、
// 权限交集全部在 nervud 的 pkgregistry 里。任何时候在本包里写出
// 「如果……就允许」都是设计错误。
package main

import (
	"encoding/binary"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"time"
)

// DefaultSockPath 是管理通道的固定路径。
//
// 与 App 控制面 /run/nervus/nervud.sock 是两条独立 socket：那条只接受 App 段
// UID，这条只接受运维身份与 pkgmanagerd。别接错。
const DefaultSockPath = "/run/nervus/nervud-admin.sock"

// 管理命令。必须与内核 adminwire 的常量逐字一致。
const (
	CmdBeginStaging  = "begin-staging"
	CmdInstall       = "install"
	CmdUninstall     = "uninstall"
	CmdList          = "list"
	CmdSetEnabled    = "set-enabled"
	CmdSetPermission = "set-permission"
)

// 授予状态的 wire 表示。
const (
	GrantStateNotRequested    = "not-requested"
	GrantStateGranted         = "granted"
	GrantStateDenied          = "denied"
	GrantStateDeniedPermanent = "denied-permanent"
)

// 结果码。
const (
	CodeOK           = "ok"
	CodeBadRequest   = "bad-request"
	CodeUnauthorized = "unauthorized"
	CodeNotFound     = "not-found"
	CodeFailed       = "failed"
)

// Request 是发往 nervud 的一条管理命令。
// 一条连接一条命令：发一个 Request、收一个 Response、随即关闭。
type Request struct {
	Cmd         string `json:"cmd"`
	StagingDir  string `json:"staging_dir,omitempty"`
	PackageID   string `json:"package_id,omitempty"`
	ComponentID string `json:"component_id,omitempty"`
	Enabled     bool   `json:"enabled,omitempty"`
	Permission  string `json:"permission,omitempty"`
	GrantState  string `json:"grant_state,omitempty"`
}

// Response 是 nervud 的应答。
type Response struct {
	OK         bool          `json:"ok"`
	Code       string        `json:"code,omitempty"`
	Message    string        `json:"message,omitempty"`
	StagingDir string        `json:"staging_dir,omitempty"`
	Package    *PackageInfo  `json:"package,omitempty"`
	Packages   []PackageInfo `json:"packages,omitempty"`
}

// PackageInfo 是一个已装 Package 的对外投影。
type PackageInfo struct {
	ID          string   `json:"id"`
	Version     string   `json:"version"`
	VersionCode uint64   `json:"version_code"`
	Trust       string   `json:"trust"`
	Source      string   `json:"source"`
	Granted     []string `json:"granted,omitempty"`
	Disabled    []string `json:"disabled,omitempty"`
}

// MaxMessageBytes 是单条 JSON 消息的硬上限，必须与内核一致。
const MaxMessageBytes = 1 << 20

const lengthPrefixBytes = 4

// ErrMessageTooLarge 长度前缀超过硬上限。
var ErrMessageTooLarge = errors.New("adminclient: message exceeds hard limit")

const (
	dialTimeout = 3 * time.Second
	ioTimeout   = 30 * time.Second
)

// Client 是管理通道客户端。零值不可用，用 New 构造。
type Client struct {
	sockPath string
}

// newAdminClient 构造客户端。sockPath 为空则用 DefaultSockPath。
func newAdminClient(sockPath string) *Client {
	if sockPath == "" {
		sockPath = DefaultSockPath
	}
	return &Client{sockPath: sockPath}
}

// Do 发一条命令并等应答。每次调用建一条新连接（管理面低频，简单胜过复用）。
func (c *Client) Do(req Request) (Response, error) {
	conn, err := net.DialTimeout("unix", c.sockPath, dialTimeout)
	if err != nil {
		return Response{}, fmt.Errorf("adminclient: dial %s: %w", c.sockPath, err)
	}
	defer func() { _ = conn.Close() }()

	if err := conn.SetDeadline(time.Now().Add(ioTimeout)); err != nil {
		return Response{}, err
	}

	// 写失败【不立即返回】，先试着读一次。nervud 在 SO_PEERCRED 判定不通过时
	// 会在读请求之前就写出拒绝并关连接，客户端这一侧完全可能在写的过程中撞上
	// EPIPE，而那条拒绝响应其实已经在缓冲区里等着。直接返回写错误的话，
	// 日志里就是「broken pipe」而不是「unauthorized」——把一个明确的权限问题
	// 伪装成网络故障。内核侧 adminwire 客户端有同样的处理。
	writeErr := writeTo(conn, req)

	var resp Response
	if err := readFrom(conn, &resp); err != nil {
		if writeErr != nil {
			return Response{}, writeErr
		}
		return Response{}, fmt.Errorf("adminclient: read response: %w", err)
	}
	return resp, nil
}

func writeTo(w io.Writer, v any) error {
	body, err := json.Marshal(v)
	if err != nil {
		return fmt.Errorf("adminclient: marshal: %w", err)
	}
	if len(body) > MaxMessageBytes {
		return fmt.Errorf("%w: %d > %d", ErrMessageTooLarge, len(body), MaxMessageBytes)
	}
	var hdr [lengthPrefixBytes]byte
	binary.BigEndian.PutUint32(hdr[:], uint32(len(body)))
	if _, err := w.Write(hdr[:]); err != nil {
		return fmt.Errorf("adminclient: write header: %w", err)
	}
	if _, err := w.Write(body); err != nil {
		return fmt.Errorf("adminclient: write body: %w", err)
	}
	return nil
}

func readFrom(r io.Reader, v any) error {
	var hdr [lengthPrefixBytes]byte
	if _, err := io.ReadFull(r, hdr[:]); err != nil {
		return err
	}
	n := binary.BigEndian.Uint32(hdr[:])
	if n == 0 {
		return errors.New("adminclient: zero-length message")
	}
	if n > MaxMessageBytes {
		// 不为对端自称的正文分配缓冲
		return fmt.Errorf("%w: %d > %d", ErrMessageTooLarge, n, MaxMessageBytes)
	}
	body := make([]byte, n)
	if _, err := io.ReadFull(r, body); err != nil {
		return err
	}
	return json.Unmarshal(body, v)
}
