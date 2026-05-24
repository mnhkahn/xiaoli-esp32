#!/usr/bin/env python3
import argparse
import json
import os
import statistics
import sys
import time
from pathlib import Path

import requests


DEFAULT_AUDIO = (
    Path(__file__).resolve().parents[2]
    / "xiaozhi-esp32"
    / "managed_components"
    / "espressif__esp-sr"
    / "esp-tts"
    / "samples"
    / "xiaoxin_speed1.wav"
)
DEFAULT_ENV_FILE = Path(__file__).resolve().parents[1] / ".env"


def now():
    return time.monotonic()


def load_env_file(path):
    if not path or not path.exists():
        return
    for line in path.read_text(encoding="utf-8").splitlines():
        line = line.strip()
        if not line or line.startswith("#") or "=" not in line:
            continue
        key, value = line.split("=", 1)
        key = key.strip()
        value = value.strip().strip('"').strip("'")
        os.environ.setdefault(key, value)


def required_env(name):
    value = os.environ.get(name)
    if not value:
        raise RuntimeError(f"missing env: {name}")
    return value


def short_json(response):
    try:
        return json.dumps(response.json(), ensure_ascii=False)[:300]
    except Exception:
        return response.text[:300]


def bench_siliconflow_asr(audio_path, model, timeout):
    key = required_env("SILICONFLOW_API_KEY")
    start = now()
    with open(audio_path, "rb") as audio:
        response = requests.post(
            "https://api.siliconflow.cn/v1/audio/transcriptions",
            headers={"Authorization": f"Bearer {key}"},
            files={"file": (Path(audio_path).name, audio, "audio/wav")},
            data={"model": model},
            timeout=timeout,
        )
    total = now() - start
    if response.status_code >= 400:
        raise RuntimeError(f"ASR failed: {response.status_code} {short_json(response)}")
    text = response.json().get("text", "")
    return {"stage": "asr", "first": None, "total": total, "size": len(text), "detail": text[:80]}


def bench_openrouter_llm(model, prompt, timeout):
    key = required_env("OPENROUTER_API_KEY")
    return bench_chat_completion(
        stage="llm",
        api_url="https://openrouter.ai/api/v1/chat/completions",
        api_key=key,
        model=model,
        prompt=prompt,
        timeout=timeout,
        extra_headers={
            "HTTP-Referer": "https://xiaoli-server.local",
            "X-Title": "xiaoli-local-benchmark",
        },
        extra_body={},
    )


def bench_siliconflow_llm(model, prompt, timeout):
    key = required_env("SILICONFLOW_API_KEY")
    return bench_chat_completion(
        stage="llm",
        api_url="https://api.siliconflow.cn/v1/chat/completions",
        api_key=key,
        model=model,
        prompt=prompt,
        timeout=timeout,
        extra_headers={},
        extra_body={"enable_thinking": False},
    )


def bench_chat_completion(stage, api_url, api_key, model, prompt, timeout, extra_headers, extra_body):
    start = now()
    first = None
    content = []
    response = requests.post(
        api_url,
        headers={
            "Authorization": f"Bearer {api_key}",
            "Content-Type": "application/json",
            **extra_headers,
        },
        json={
            "model": model,
            "messages": [
                {"role": "system", "content": "You are XiaoLi. Reply briefly in Chinese."},
                {"role": "user", "content": prompt},
            ],
            "stream": True,
            "max_tokens": 80,
            "temperature": 0.2,
            **extra_body,
        },
        stream=True,
        timeout=timeout,
    )
    if response.status_code >= 400:
        raise RuntimeError(f"LLM failed: {response.status_code} {response.text[:300]}")
    response.encoding = "utf-8"

    for raw_line in response.iter_lines(decode_unicode=True):
        if not raw_line:
            continue
        if not raw_line.startswith("data: "):
            continue
        data = raw_line[6:]
        if data == "[DONE]":
            break
        payload = json.loads(data)
        delta = payload.get("choices", [{}])[0].get("delta", {})
        piece = delta.get("content") or ""
        if piece and first is None:
            first = now() - start
        content.append(piece)
    total = now() - start
    text = "".join(content)
    return {"stage": stage, "first": first, "total": total, "size": len(text), "detail": text[:80]}


def bench_siliconflow_tts(model, voice, text, response_format, stream, timeout):
    key = required_env("SILICONFLOW_API_KEY")
    start = now()
    first = None
    size = 0
    payload = {
        "model": model,
        "input": text,
        "voice": voice,
        "response_format": response_format,
    }
    if stream:
        payload["stream"] = True
    response = requests.post(
        "https://api.siliconflow.cn/v1/audio/speech",
        headers={"Authorization": f"Bearer {key}", "Content-Type": "application/json"},
        json=payload,
        stream=stream,
        timeout=timeout,
    )
    if response.status_code >= 400:
        raise RuntimeError(f"TTS failed: {response.status_code} {short_json(response)}")

    if stream:
        for chunk in response.iter_content(chunk_size=4096):
            if not chunk:
                continue
            if first is None:
                first = now() - start
            size += len(chunk)
    else:
        size = len(response.content)
        first = now() - start
    total = now() - start
    return {
        "stage": "tts_stream" if stream else "tts_full",
        "first": first,
        "total": total,
        "size": size,
        "detail": f"{model} / {voice} / {response_format}",
    }


def fmt(seconds):
    if seconds is None:
        return "-"
    return f"{seconds:.3f}s"


def print_result(result):
    print(
        f"{result['stage']:<10} first={fmt(result['first']):>8} "
        f"total={fmt(result['total']):>8} size={result['size']:<8} {result['detail']}"
    , flush=True)


def print_error(stage, exc):
    print(f"{stage:<10} error={str(exc)[:180]}", flush=True)


def summarize(results):
    by_stage = {}
    for item in results:
        by_stage.setdefault(item["stage"], []).append(item)
    print("\nsummary")
    for stage, items in by_stage.items():
        firsts = [x["first"] for x in items if x["first"] is not None]
        totals = [x["total"] for x in items]
        first = statistics.median(firsts) if firsts else None
        total = statistics.median(totals) if totals else None
        print(f"{stage:<10} median_first={fmt(first):>8} median_total={fmt(total):>8}")


def main():
    parser = argparse.ArgumentParser()
    parser.add_argument("--env-file", default=str(DEFAULT_ENV_FILE))
    parser.add_argument("--runs", type=int, default=1)
    parser.add_argument("--audio", default=str(DEFAULT_AUDIO))
    parser.add_argument("--text", default="你好，我是小李。")
    parser.add_argument("--prompt", default="你听得到吗？")
    parser.add_argument("--timeout", type=float, default=60)
    parser.add_argument("--asr-model")
    parser.add_argument("--llm-provider", choices=["openrouter", "siliconflow"], default=os.environ.get("LLM_PROVIDER", "openrouter"))
    parser.add_argument("--llm-model")
    parser.add_argument("--tts-model")
    parser.add_argument("--tts-voice")
    parser.add_argument("--tts-format")
    parser.add_argument("--skip-asr", action="store_true")
    parser.add_argument("--skip-llm", action="store_true")
    parser.add_argument("--skip-tts", action="store_true")
    args = parser.parse_args()
    load_env_file(Path(args.env_file) if args.env_file else None)
    args.asr_model = args.asr_model or os.environ.get("SILICONFLOW_ASR_MODEL", "FunAudioLLM/SenseVoiceSmall")
    if args.llm_model:
        pass
    elif args.llm_provider == "siliconflow":
        args.llm_model = os.environ.get("SILICONFLOW_LLM_MODEL", "Qwen/Qwen3-8B")
    else:
        args.llm_model = os.environ.get("OPENROUTER_LLM_MODEL", "openrouter/free")
    args.tts_model = args.tts_model or os.environ.get("SILICONFLOW_TTS_MODEL", "FunAudioLLM/CosyVoice2-0.5B")
    args.tts_voice = args.tts_voice or os.environ.get("SILICONFLOW_TTS_VOICE", "FunAudioLLM/CosyVoice2-0.5B:anna")
    args.tts_format = args.tts_format or os.environ.get("SILICONFLOW_TTS_RESPONSE_FORMAT", "mp3")

    results = []
    audio_path = Path(args.audio)
    if not audio_path.exists() and not args.skip_asr:
        raise RuntimeError(f"audio file does not exist: {audio_path}")

    for index in range(args.runs):
        print(f"run {index + 1}/{args.runs}", flush=True)
        if not args.skip_asr:
            try:
                result = bench_siliconflow_asr(audio_path, args.asr_model, args.timeout)
                results.append(result)
                print_result(result)
            except Exception as exc:
                print_error("asr", exc)
        if not args.skip_llm:
            try:
                if args.llm_provider == "siliconflow":
                    result = bench_siliconflow_llm(args.llm_model, args.prompt, args.timeout)
                else:
                    result = bench_openrouter_llm(args.llm_model, args.prompt, args.timeout)
                results.append(result)
                print_result(result)
            except Exception as exc:
                print_error("llm", exc)
        if not args.skip_tts:
            try:
                result = bench_siliconflow_tts(args.tts_model, args.tts_voice, args.text, args.tts_format, False, args.timeout)
                results.append(result)
                print_result(result)
            except Exception as exc:
                print_error("tts_full", exc)
            try:
                result = bench_siliconflow_tts(args.tts_model, args.tts_voice, args.text, args.tts_format, True, args.timeout)
                results.append(result)
                print_result(result)
            except Exception as exc:
                print_error("tts_stream", exc)

    summarize(results)
    return 0


if __name__ == "__main__":
    try:
        raise SystemExit(main())
    except Exception as exc:
        print(f"error: {exc}", file=sys.stderr)
        raise SystemExit(1)
