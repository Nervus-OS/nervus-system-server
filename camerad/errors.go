//go:build linux

// 本文件构造带 typed error_detail 的失败。
//
// # 为什么不直接 return fmt.Errorf
//
// SDK 把任意 Go error 归一化成 INTERNAL，原文只进本地日志——那是对的，
// 把内部字符串（设备节点路径、驱动错误码）塞给调用方等于把实现细节泄漏出
// 进程边界。
//
// 代价是调用方拿到的全是 INTERNAL，无从区分「这个分辨率不支持」（改参数重试
// 就能成）和「摄像头掉线了」（重试多少次都一样）。CameraErrorDetail 就是用来
// 补回这个区分的：code 说该不该重试，detail 说为什么。
package main

import (
	camerav1 "github.com/nervus-os/nervus-ipc/protocol/interface/camerav1"
	ipcv1 "github.com/nervus-os/nervus-ipc/protocol/ipcv1"
	"github.com/nervus-os/nervus-ipc/sdk"
	"google.golang.org/protobuf/proto"
)

// cameraError 构造一个带 CameraErrorDetail 的失败。
//
// control 只在 CONTROL_* 系列 reason 下有意义，其余传 UNSPECIFIED。
func cameraError(
	code ipcv1.StatusCode,
	reason camerav1.CameraReason,
	control camerav1.ControlKind,
) error {
	detail, err := proto.Marshal(&camerav1.CameraErrorDetail{
		Reason:  reason,
		Control: control,
	})
	if err != nil {
		// detail 编不出来不该让整个调用变成一个没有原因的 INTERNAL：
		// code 本身已经携带了最要紧的信息（该不该重试），保住它。
		return &sdk.StatusError{Code: code}
	}
	return &sdk.StatusError{
		Code: code,
		// 【PublicMessage 留空】：协议禁止 nervud 转发 Provider 的自由文本，
		// 填了也不会到达调用方，只会让人以为它有用。
		Detail: detail,
	}
}

// deviceUnavailable 是「设备此刻用不了」的统一形态：没插、掉线、被占。
//
// 三种情况归一是刻意的：调用方能做的事完全相同（等一会儿重试或提示用户检查
// 连接），而区分它们需要暴露「这台机器上有没有这个设备」——那属于
// nervus.interface.resource.directory 的职责，不该从一次 OpenStream 失败里
// 侧信道泄漏出去。
func deviceUnavailable() error {
	return cameraError(
		ipcv1.StatusCode_STATUS_CODE_UNAVAILABLE,
		camerav1.CameraReason_CAMERA_REASON_DEVICE_UNAVAILABLE,
		camerav1.ControlKind_CONTROL_KIND_UNSPECIFIED)
}
