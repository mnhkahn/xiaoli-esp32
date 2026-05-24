import os
from pathlib import Path


PLUGIN_PATH = Path(
    os.environ.get(
        "WEATHER_PLUGIN_PATH",
        "/opt/xiaozhi-esp32-server/plugins_func/functions/get_weather.py",
    )
)


def patch_once(text: str, old: str, new: str) -> str:
    if new in text:
        return text
    if old not in text:
        raise RuntimeError(f"patch target not found in {PLUGIN_PATH}")
    return text.replace(old, new, 1)


helper_code = r'''
def fetch_api_weather(path, location_id, api_key, api_host):
    url = f"https://{api_host}{path}"
    headers = {**HEADERS, "X-QW-Api-Key": api_key}
    response = requests.get(
        url,
        params={"location": location_id},
        headers=headers,
        timeout=10,
    )
    payload = response.json()
    if payload.get("code") != "200":
        detail = payload.get("error", {}).get("detail") or payload.get("code")
        logger.bind(tag=TAG).error(f"获取天气失败，原因：{detail}")
        return None
    return payload


def build_api_weather_report(location_name, now_payload, forecast_payload):
    now = now_payload.get("now", {})
    report = (
        f"您查询的位置是：{location_name}\n\n"
        f"当前天气: {now.get('text', '未知')}，"
        f"{now.get('temp', '未知')}℃，体感{now.get('feelsLike', '未知')}℃\n"
    )

    details = []
    if now.get("windDir") or now.get("windScale"):
        details.append(f"{now.get('windDir', '')}{now.get('windScale', '')}级")
    if now.get("humidity"):
        details.append(f"湿度{now.get('humidity')}%")
    if now.get("precip"):
        details.append(f"降水{now.get('precip')}mm")
    if now.get("vis"):
        details.append(f"能见度{now.get('vis')}km")
    if details:
        report += "详细参数：" + "，".join(details) + "\n"

    daily = forecast_payload.get("daily", []) if forecast_payload else []
    if daily:
        report += "\n未来7天预报：\n"
        for day in daily[:7]:
            report += (
                f"{day.get('fxDate')}: 白天{day.get('textDay', '未知')}，"
                f"夜间{day.get('textNight', '未知')}，"
                f"气温 {day.get('tempMin', '未知')}~{day.get('tempMax', '未知')}℃\n"
            )

    report += "\n（如需某一天的具体天气，请告诉我日期）"
    return report


def fetch_weather_by_location_id(location_id, api_key, api_host, location_name):
    now_payload = fetch_api_weather("/v7/weather/now", location_id, api_key, api_host)
    if not now_payload:
        return None
    forecast_payload = fetch_api_weather("/v7/weather/7d", location_id, api_key, api_host)
    if not forecast_payload:
        return None
    return build_api_weather_report(location_name, now_payload, forecast_payload)


'''


text = PLUGIN_PATH.read_text(encoding="utf-8")

old_description = '''            "获取某个地点的天气，用户应提供一个位置，比如用户说杭州天气，参数为：杭州。"
            "如果用户说的是省份，默认用省会城市。如果用户说的不是省份或城市而是一个地名，默认用该地所在省份的省会城市。"
            "重要：本地未来7天天气已在上下文中提供，用户未指明其他城市时绝对不要调用此工具。"
'''

new_description = '''            "获取天气信息。用户询问天气、气温、下雨、未来几天天气、本地天气、今天天气时必须调用此工具。"
            "如果用户没有指定地点，或说本地/这里/附近/海淀，location参数不要传，服务端会使用默认位置。"
            "如果用户指定其他城市或地点，例如杭州天气，则location传入用户说的地点名，例如杭州。"
'''

text = patch_once(text, old_description, new_description)

text = patch_once(text, "@register_function", helper_code + "@register_function")

old_config_block = '''    api_host = weather_config.get("api_host", "mj7p3y7naa.re.qweatherapi.com")
    api_key = weather_config.get("api_key", "a861d0d5e7bf4ee1a83d9a9e4f96d4da")
    default_location = weather_config.get("default_location", "广州")
    client_ip = conn.client_ip

    # 优先使用用户提供的location参数
    if not location:
        # 通过客户端IP解析城市
        if client_ip:
            # 先从缓存获取IP对应的城市信息
            cached_ip_info = cache_manager.get(CacheType.IP_INFO, client_ip)
            if cached_ip_info:
                location = cached_ip_info.get("city")
            else:
                # 缓存未命中，调用API获取
                ip_info = get_ip_info(client_ip, logger)
                if ip_info:
                    cache_manager.set(CacheType.IP_INFO, client_ip, ip_info)
                    location = ip_info.get("city")

            if not location:
                location = default_location
        else:
            # 若无IP，使用默认位置
            location = default_location
'''

new_config_block = '''    api_host = weather_config.get("api_host", "mj7p3y7naa.re.qweatherapi.com")
    api_key = weather_config.get("api_key", "a861d0d5e7bf4ee1a83d9a9e4f96d4da")
    default_location = weather_config.get("default_location", "广州")
    default_location_id = weather_config.get("default_location_id")
    default_location_name = weather_config.get("default_location_name", default_location)
    prefer_default_location = weather_config.get("prefer_default_location", True)
    client_ip = conn.client_ip

    using_default_location = False
    # 服务端已配置固定设备位置时，优先使用固定位置，避免 Fly 边缘 IP 或运营商 IP 造成定位漂移。
    if not location and prefer_default_location:
        location = default_location_id or default_location
        using_default_location = True
    elif not location:
        # 通过客户端IP解析城市
        if client_ip:
            # 先从缓存获取IP对应的城市信息
            cached_ip_info = cache_manager.get(CacheType.IP_INFO, client_ip)
            if cached_ip_info:
                location = cached_ip_info.get("city")
            else:
                # 缓存未命中，调用API获取
                ip_info = get_ip_info(client_ip, logger)
                if ip_info:
                    cache_manager.set(CacheType.IP_INFO, client_ip, ip_info)
                    location = ip_info.get("city")

            if not location:
                location = default_location
                using_default_location = True
        else:
            # 若无IP，使用默认位置
            location = default_location
            using_default_location = True
'''

text = patch_once(text, old_config_block, new_config_block)

old_fetch_block = '''    # 缓存未命中，获取实时天气数据
    city_info = fetch_city_info(location, api_key, api_host)
'''

new_fetch_block = '''    # 缓存未命中，优先用固定 LocationID 直接查天气，跳过 GeoAPI。
    direct_location_id = None
    if default_location_id:
        location_text = str(location or "").strip()
        default_location_aliases = {
            "",
            "本地",
            "当地",
            "这里",
            "这边",
            "附近",
            "当前",
            "当前位置",
            "所在城市",
            "当前城市",
            "默认位置",
            "未知位置",
            "未知城市",
        }
        if (
            using_default_location
            or location_text in default_location_aliases
            or "未知" in location_text
            or location_text == default_location_id
            or location_text == default_location
            or location_text == default_location_name
            or "海淀" in location_text
        ):
            direct_location_id = default_location_id

    if direct_location_id:
        weather_report = fetch_weather_by_location_id(
            direct_location_id, api_key, api_host, default_location_name
        )
        if weather_report:
            cache_manager.set(CacheType.WEATHER, weather_cache_key, weather_report)
            return ActionResponse(Action.REQLLM, weather_report, None)

    # 缓存未命中，获取实时天气数据
    city_info = fetch_city_info(location, api_key, api_host)
'''

text = patch_once(text, old_fetch_block, new_fetch_block)

PLUGIN_PATH.write_text(text, encoding="utf-8")
