#!/usr/bin/env bash
# 把本仓库的系统服务构建、打包、签名，产出可直接放进
# /usr/lib/nervus/system-packages/ 的目录树。
#
# 用法：
#   scripts/build-image-tree.sh <输出目录> <签名私钥.pem> [ABI]
#
# ABI 默认 linux-arm64（目标机 RK3588）。开发机上跑 nervud 时用 linux-x86_64。
#
# 产物形态是【解包后的目录树】，不是 .nspkg —— 内核的启动扫描 glob 的是
# <systemPackagesDir>/*/manifest.json。.nspkg 是动态安装（nervusctl install）
# 用的 zstd+tar 格式，两条路不能混。
#
# 生成私钥：
#   openssl genpkey -algorithm ed25519 -out signing/platform-release.pem

set -euo pipefail

OUT="${1:?用法: $0 <输出目录> <私钥.pem> [ABI]}"
KEY="${2:?用法: $0 <输出目录> <私钥.pem> [ABI]}"
ABI="${3:-linux-arm64}"

cd "$(dirname "$0")/.."
REPO="$PWD"

# ABI token → GOARCH。内核只认这三个 canonical token，不接受 Android NDK 名
# （arm64-v8a）或裸 CPU 名（aarch64），也不做归一化，所以这里也不做。
case "$ABI" in
	linux-arm64)   GOARCH=arm64 ;;
	linux-armv7)   GOARCH=arm   ;;
	linux-x86_64)  GOARCH=amd64 ;;
	*) echo "不支持的 ABI: $ABI（只接受 linux-arm64 / linux-armv7 / linux-x86_64）" >&2; exit 1 ;;
esac

# 本仓库的全部系统服务：<package_id>:<服务目录名>
#
# 服务目录名同时是二进制名与 Go 包路径（./​<dir>），manifest 模板固定在
# <dir>/manifest.json.in —— 一个服务的一切都在它自己那个目录里。
# 加服务时在这里加一行即可。
SERVICES=(
	"nervus.pkgmanagerd:pkgmanagerd"
	"nervus.safety.recovery:safetyrecoveryd"
)

echo "==> 目标 ABI: $ABI (GOARCH=$GOARCH)"
mkdir -p "$OUT"

for entry in "${SERVICES[@]}"; do
	pkg="${entry%%:*}"
	svc="${entry##*:}"
	tmpl="$REPO/$svc/manifest.json.in"

	if [[ ! -f "$tmpl" ]]; then
		echo "缺少 manifest 模板: $tmpl" >&2
		exit 1
	fi

	# 输出目录名【必须】等于 package_id：内核按 manifest 内容认包，目录名只是
	# 容器，但两者不一致会让人在排查时看着目录名找错包。
	dir="$OUT/$pkg"
	echo "==> 构建 $pkg (./$svc)"
	rm -rf "$dir"
	mkdir -p "$dir/bin"

	# CGO_ENABLED=0：系统服务必须是静态的。目标机的 glibc 版本与构建机未必
	# 一致，而系统包的沙箱是 ProtectSystem=strict，动态链接一旦缺库，
	# 表现是「进程起来就退出」，systemd 只报退出码，排查起来很费劲。
	CGO_ENABLED=0 GOOS=linux GOARCH="$GOARCH" \
		go build -trimpath -ldflags "-s -w" -o "$dir/bin/$svc" "./$svc"

	# 以命令行的 ABI 为准——同一份模板要能构建多个 ABI。
	#
	# 【临时模板必须落在包目录之外】。放进 $dir 的话，sysmanifest 会把它算进
	# digests（那时它还在），事后删掉就变成 digests 里有一条指向不存在文件的
	# 记录，内核 VerifyDigests 直接以 Missing 拒绝整个包 —— 而错误信息看起来
	# 像「镜像损坏」，足以让人先去查硬件。
	tmplTmp="$(mktemp)"
	trap 'rm -f "$tmplTmp"' EXIT
	sed "s/\"linux-arm64\"/\"$ABI\"/" "$tmpl" > "$tmplTmp"

	go run ./scripts/sysmanifest -dir "$dir" -template "$tmplTmp" -key "$KEY"
	rm -f "$tmplTmp"
done

echo
echo "==> 产物"
find "$OUT" -type f | sort | sed 's|^|  |'
echo
