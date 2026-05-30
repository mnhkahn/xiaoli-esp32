#!/bin/bash
# ============================================================
# Xiaoli ESP32-S3 编译烧录脚本
# 项目: xiaozhi v2.2.6 (ESP-IDF)
#
# 用法:
#   ./flash.sh              只烧录（需先编译过）
#   ./flash.sh --build      全量编译 + 烧录
#   ./flash.sh --port /dev/cu.xxx  指定串口
# ============================================================
set -e

SCRIPT_DIR="$(cd "$(dirname "$0")" && pwd)"
BUILD_DIR="$SCRIPT_DIR/xiaozhi-esp32/build"
ESPTOOL="$SCRIPT_DIR/.espressif/python_env/idf5.5_py3.13_env/bin/esptool.py"
BAUD="${BAUD:-460800}"
DO_BUILD=false
PORT=""

# ---------- 参数解析 ----------
while [[ $# -gt 0 ]]; do
    case "$1" in
        -b|--build) DO_BUILD=true; shift ;;
        -p|--port)  PORT="$2"; shift 2 ;;
        *)          echo "Usage: $0 [--build] [--port /dev/cu.xxx]"; exit 1 ;;
    esac
done

# ---------- 编译 ----------
if [ "$DO_BUILD" = true ]; then
    echo "========================================"
    echo "  Full clean + Build"
    echo "========================================"
    export IDF_PYTHON_ENV_PATH="$SCRIPT_DIR/.espressif/python_env/idf5.5_py3.13_env"
    export IDF_TOOLS_PATH="$SCRIPT_DIR/.espressif"
    source "$SCRIPT_DIR/esp-idf/export.sh" > /dev/null 2>&1

    cd "$SCRIPT_DIR/xiaozhi-esp32"
    echo "Cleaning..."
    idf.py fullclean
    echo "Building..."
    idf.py build
    cd "$SCRIPT_DIR"
    echo "Build complete."
    echo ""
fi

# ---------- 检查 esptool ----------
if [ ! -f "$ESPTOOL" ]; then
    echo "esptool.py not found: $ESPTOOL"
    echo "   Run: source .espressif/python_env/idf5.5_py3.13_env/bin/activate"
    exit 1
fi

# ---------- 串口选择 ----------
if [ -n "$PORT" ]; then
    SELECTED_PORT="$PORT"
else
    mapfile -t PORTS < <(ls /dev/cu.usb* /dev/cu.usbserial* /dev/cu.SLAB* /dev/cu.wch* 2>/dev/null | grep -vi bluetooth || true)

    if [ ${#PORTS[@]} -eq 0 ]; then
        echo "No serial ports found. Is the device plugged in?"
        exit 1
    fi

    if [ ${#PORTS[@]} -eq 1 ]; then
        SELECTED_PORT="${PORTS[0]}"
        echo "Auto-detected: $SELECTED_PORT"
    else
        echo ""
        echo "Multiple serial ports found:"
        echo ""
        i=1
        for p in "${PORTS[@]}"; do
            echo "  [$i]  $p"
            ((i++))
        done
        echo ""
        read -rp "Select port [1-$((i-1))]: " choice

        if ! [[ "$choice" =~ ^[0-9]+$ ]] || [ "$choice" -lt 1 ] || [ "$choice" -gt $((i-1)) ]; then
            echo "Invalid selection."
            exit 1
        fi
        SELECTED_PORT="${PORTS[$((choice-1))]}"
    fi
fi

if [ ! -e "$SELECTED_PORT" ]; then
    echo "Serial port not found: $SELECTED_PORT"
    exit 1
fi

# ---------- 检查 bin 文件 ----------
check_bin() {
    local name="$1"
    local path="$BUILD_DIR/$2"
    if [ ! -f "$path" ]; then
        echo "Missing: $name ($path)"
        echo "   Run: ./flash.sh --build"
        exit 1
    fi
}

check_bin "bootloader"       "bootloader/bootloader.bin"
check_bin "partition table"  "partition_table/partition-table.bin"
check_bin "ota data"         "ota_data_initial.bin"
check_bin "xiaozhi app"      "xiaozhi.bin"

ASSETS="$SCRIPT_DIR/xiaozhi-esp32/main/boards/bread-compact-wifi-s3cam/assets.bin"
if [ ! -f "$ASSETS" ]; then
    echo "Missing: assets.bin ($ASSETS)"
    exit 1
fi

# ---------- 确认烧录 ----------
echo ""
echo "========================================"
echo "  Xiaoli ESP32-S3 Flash Tool"
echo "========================================"
echo "  Chip:       esp32s3"
echo "  Port:       $SELECTED_PORT"
echo "  Baud:       $BAUD"
echo "  Flash:      dio / 16MB / 80MHz"
echo "  Bootloader: $(du -h "$BUILD_DIR/bootloader/bootloader.bin" | cut -f1)"
echo "  App:        $(du -h "$BUILD_DIR/xiaozhi.bin" | cut -f1)"
echo "  Assets:     $(du -h "$ASSETS" | cut -f1)"
echo "========================================"
echo ""
read -rp "Start flashing? [Y/n] " confirm
if [[ "$confirm" =~ ^[Nn] ]]; then
    echo "Aborted."
    exit 0
fi
echo ""

# ---------- 烧录 ----------
"$ESPTOOL" \
    --chip esp32s3 \
    --port "$SELECTED_PORT" \
    --baud "$BAUD" \
    --before default_reset \
    --after hard_reset \
    write_flash \
    --flash_mode dio \
    --flash_size 16MB \
    --flash_freq 80m \
    0x0 "$BUILD_DIR/bootloader/bootloader.bin" \
    0x8000 "$BUILD_DIR/partition_table/partition-table.bin" \
    0xd000 "$BUILD_DIR/ota_data_initial.bin" \
    0x20000 "$BUILD_DIR/xiaozhi.bin" \
    0x7e0000 "$ASSETS"

echo ""
echo "========================================"
echo "  Flash complete!"
echo "========================================"
