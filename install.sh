#!/bin/bash
set -e

APP_DIR="/opt/llm-gateway"
SERVICE_NAME="llm-gateway"
BINARY="llm_api_gateway"

echo "=== LLM API Gateway 一键部署 ==="
echo ""

# Check for root
if [ "$EUID" -ne 0 ]; then
    echo "请使用 sudo 运行此脚本"
    exit 1
fi

# 1. Build
echo "[1/7] 编译..."
CGO_ENABLED=0 go build -ldflags="-s -w" -o "$BINARY" .
echo "  ✓ 编译完成"

# 2. Create directories
echo "[2/7] 创建目录..."
mkdir -p "$APP_DIR"
echo "  ✓ $APP_DIR"

# 3. Copy files
echo "[3/7] 复制文件..."
cp "$BINARY" "$APP_DIR/"
if [ ! -f "$APP_DIR/config.yaml" ]; then
    cp config.yaml.example "$APP_DIR/config.yaml"
    echo "  ✓ 已复制 config.yaml（请编辑设置真实 API Key）"
else
    echo "  ✓ config.yaml 已存在，跳过"
fi
cp deploy/llm-gateway.service /etc/systemd/system/
echo "  ✓ systemd 服务文件已安装"

# 4. Create user
echo "[4/7] 创建 llm 用户..."
id -u llm &>/dev/null || useradd -r -s /bin/false llm
chown -R llm:llm "$APP_DIR"
echo "  ✓ llm 用户已就绪"

# 5. Set permissions
echo "[5/7] 设置文件权限..."
chmod 600 "$APP_DIR/config.yaml"
chmod 700 "$APP_DIR"
echo "  ✓ 权限已加固"

# 6. Start service
echo "[6/7] 启动服务..."
systemctl daemon-reload
systemctl enable "$SERVICE_NAME"
systemctl start "$SERVICE_NAME"
echo "  ✓ 服务已启动"

# 7. Initialize admin
echo "[7/7] 初始化 Admin 密码..."
echo -n "请输入 Admin 密码: "
read -s ADMIN_PASS
echo ""
if [ -n "$ADMIN_PASS" ]; then
    "$APP_DIR/$BINARY" -config "$APP_DIR/config.yaml" -passwd "$ADMIN_PASS"
    echo "  ✓ Admin 密码已设置"
else
    echo "  ⚠ 跳过密码设置（稍后可通过 make init-admin 设置）"
fi

echo ""
echo "=== 部署完成 ==="
echo "管理面板: https://$(hostname -I | awk '{print $1}')/admin/"
echo "API 端点:  https://$(hostname -I | awk '{print $1}')/v1/chat/completions"
echo ""
echo "请确保已配置 Nginx 并编辑 $APP_DIR/config.yaml 中的 API Key"
