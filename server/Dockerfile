FROM golang:1.23 AS go-build

WORKDIR /src
COPY go.mod ./
COPY cmd/ cmd/
COPY internal/ internal/
RUN go build -o /out/xiaoli-admin ./cmd/xiaoli-admin

FROM ghcr.nju.edu.cn/xinnan-tech/xiaozhi-esp32-server:server_latest

USER root

RUN apt-get update \
    && apt-get install -y --no-install-recommends nginx ca-certificates \
    && rm -rf /var/lib/apt/lists/*

RUN pip install --no-cache-dir langsmith

RUN python -c "from pathlib import Path; p=Path('/opt/xiaozhi-esp32-server/core/providers/llm/openai/openai.py'); s=p.read_text(); needle='    \"aliyuncs.com\": {\"enable_thinking\": False},\\n'; insert='    \"siliconflow.cn\": {\"enable_thinking\": False},\\n'; p.write_text(s if insert in s else s.replace(needle, needle + insert))"

COPY fly/nginx.conf /fly/nginx.conf
COPY fly/entrypoint.py /fly/entrypoint.py
COPY fly/config.template.yaml /fly/config.template.yaml
COPY fly/patch_weather_plugin.py /fly/patch_weather_plugin.py
COPY --from=go-build /out/xiaoli-admin /usr/local/bin/xiaoli-admin
COPY fly/xiaoli_bridge.py /opt/xiaozhi-esp32-server/xiaoli_bridge.py
COPY fly/xiaoli_langsmith.py /opt/xiaozhi-esp32-server/xiaoli_langsmith.py
COPY fly/patch_admin.py /fly/patch_admin.py
COPY fly/patch_langsmith.py /fly/patch_langsmith.py
COPY fly/patch_device_stream.py /fly/patch_device_stream.py

RUN python /fly/patch_weather_plugin.py
RUN python /fly/patch_admin.py
RUN python /fly/patch_langsmith.py
RUN python /fly/patch_device_stream.py

EXPOSE 8080

CMD ["python", "/fly/entrypoint.py"]
