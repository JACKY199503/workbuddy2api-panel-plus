#!/usr/bin/env bash
# 打印 wb2api 面板/接口用的 API Key
grep -o '"api_key"[^,]*' /opt/wb2api/config/config.json
