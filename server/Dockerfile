FROM ghcr.nju.edu.cn/xinnan-tech/xiaozhi-esp32-server:server_latest

USER root

RUN apt-get update \
    && apt-get install -y --no-install-recommends nginx ca-certificates \
    && rm -rf /var/lib/apt/lists/*

RUN python -c "from pathlib import Path; p=Path('/opt/xiaozhi-esp32-server/core/providers/llm/openai/openai.py'); s=p.read_text(); needle='    \"aliyuncs.com\": {\"enable_thinking\": False},\\n'; insert='    \"siliconflow.cn\": {\"enable_thinking\": False},\\n'; p.write_text(s if insert in s else s.replace(needle, needle + insert))"

COPY fly/nginx.conf /fly/nginx.conf
COPY fly/entrypoint.py /fly/entrypoint.py
COPY fly/config.template.yaml /fly/config.template.yaml
COPY fly/patch_weather_plugin.py /fly/patch_weather_plugin.py
COPY fly/xiaoli_admin.py /opt/xiaozhi-esp32-server/xiaoli_admin.py
COPY fly/patch_admin.py /fly/patch_admin.py

RUN python /fly/patch_weather_plugin.py
RUN python /fly/patch_admin.py

EXPOSE 8080

CMD ["python", "/fly/entrypoint.py"]
