# ESP32-S3 固件烧录说明

## 烧录命令

```bash
source /Users/mnhkahn/code/xiaoli-esp32/esp-idf/export.sh && \
cd /Users/mnhkahn/code/xiaoli-esp32/xiaozhi-esp32 && \
idf.py -p /dev/cu.usbserial-14310 flash
```

- `source .../export.sh` — 加载 ESP-IDF 环境变量
- `cd .../xiaozhi-esp32` — 进入项目目录（CMakeLists.txt 所在位置）
- `idf.py` — ESP-IDF 官方提供的构建/烧录命令行工具
- `-p /dev/cu.usbserial-14310` — 指定串口设备（插拔 USB 后端口号可能变化，用 `ls /dev/cu.usbserial-*` 确认）
- `flash` — 编译并烧录（等于 `build` + 烧录）

## 烧录区域

| 地址 | 文件 | 内容 |
|------|------|------|
| 0x0 | bootloader.bin | 引导加载程序 |
| 0x8000 | partition-table.bin | 分区表 |
| 0xd000 | ota_data_initial.bin | OTA 启动分区标记 |
| 0x20000 | xiaozhi.bin | 主程序固件 |
| 0x7e0000 | assets.bin | 资源文件（语音提示音、多语言等） |

## 启动执行顺序

```
ESP32-S3 上电
    ↓
1. 芯片内部 ROM（出厂固化）
    ↓ 读取 Flash 0x0
2. bootloader.bin（0x0）— 初始化硬件，读取分区表
    ↓
3. partition-table.bin（0x8000）— 定义 Flash 各区域划分
    ↓ 找到 OTA 分区，读取 ota_data
4. ota_data_initial.bin（0xd000）— 确定启动固件分区 A 或 B
    ↓
5. xiaozhi.bin（0x20000）— 运行应用代码
    ↓ 启动时加载资源
6. assets.bin（0x7e0000）— 语音提示音、多语言字符串
```

- 步骤 1-4：ESP-IDF 框架标准启动流程
- 步骤 5-6：我们的应用代码和资源

## 硬件信息

- 芯片：ESP32-S3，16MB Flash
- 板子：bread-compact-wifi-s3cam
- 功放：NS4168（I2S D 类功放，SDB 接 3.3V 常开）
- 摄像头：OV3660
- Flash 模式：DIO，频率 80MHz
- 烧录速率：460800 bps

## 常用串口操作

```bash
# 查看可用串口
ls /dev/cu.usbserial-*

# 仅编译不烧录
idf.py build

# 重启板子
python3 -m esptool --port /dev/cu.usbserial-14310 run

# 查看串口日志
python3 -c "import serial; s=serial.Serial('/dev/cu.usbserial-14310',115200,timeout=1); [print(s.read(4096).decode('utf-8',errors='replace'),end='') for _ in iter(lambda:s.read(4096),b'')]"
```
