//go:build linux

// 本文件是 V4L2 fourcc 与平台 PixelFormat 之间的双向映射。
//
// # 为什么必须双向、且必须显式
//
// 单向（只 V4L2→平台）会让 OpenStream 无法把调用方请求的格式翻回去，于是只能
// 按名字猜；双向但用一张表反查，则要求映射是双射——而它确实是：一个平台格式
// 对应一种确定的内存布局，一个 fourcc 也是。两张表让「新增一个格式却只加了
// 一半」这件事在编译期看不出来，所以下面有一个测试逐项核对两边。
//
// # 不认识的格式一律【跳过】，不映射成 UNSPECIFIED
//
// 把未知 fourcc 报成 UNSPECIFIED，App 会拿到一个自己无法解释布局的格式，
// 然后按它去解析共享内存——读出来的是花屏，而 DescribeStream 明明「成功」了。
// 跳过的话 App 只是看不到这个格式，行为是可预期的。
package main

import camerav1 "github.com/nervus-os/nervus-ipc/protocol/interface/camerav1"

// pixelFormatTable 是唯一的映射真源，两个方向的查表都从它派生。
var pixelFormatTable = []struct {
	fourcc uint32
	proto  camerav1.PixelFormat
}{
	{v4l2PixFmtNV12, camerav1.PixelFormat_PIXEL_FORMAT_NV12},
	{v4l2PixFmtYUYV, camerav1.PixelFormat_PIXEL_FORMAT_YUYV},
	{v4l2PixFmtMJPEG, camerav1.PixelFormat_PIXEL_FORMAT_MJPEG},
	{v4l2PixFmtRGB24, camerav1.PixelFormat_PIXEL_FORMAT_RGB888},
	{v4l2PixFmtZ16, camerav1.PixelFormat_PIXEL_FORMAT_DEPTH16},
}

// pixelFormatOf 把 V4L2 fourcc 翻成平台格式。不认识返回 ok=false。
func pixelFormatOf(fourcc uint32) (camerav1.PixelFormat, bool) {
	for _, entry := range pixelFormatTable {
		if entry.fourcc == fourcc {
			return entry.proto, true
		}
	}
	return camerav1.PixelFormat_PIXEL_FORMAT_UNSPECIFIED, false
}

// fourccOf 把平台格式翻回 V4L2 fourcc。不认识返回 ok=false。
//
// UNSPECIFIED 【必须】返回 false：它是「调用方没填」的意思，不是一种格式。
// 放行会让一次漏填变成一次按零值 fourcc 去 S_FMT 的调用。
func fourccOf(format camerav1.PixelFormat) (uint32, bool) {
	if format == camerav1.PixelFormat_PIXEL_FORMAT_UNSPECIFIED {
		return 0, false
	}
	for _, entry := range pixelFormatTable {
		if entry.proto == format {
			return entry.fourcc, true
		}
	}
	return 0, false
}
