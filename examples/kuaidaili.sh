#!/bin/sh
# 快代理（kuaidaili.com）私密代理取 IP 脚本，供 ProxyRotation 的 source=command 使用。
# 成功时向 stdout 打印一行完整的代理 URL（socks5://user:pass@ip:port），
# 失败时向 stderr 说明原因并以非 0 退出。
#
# 用 hmacsha1 而不是默认的 token 签名：token 方式要先 POST 一次 get_secret_token，
# 而令牌有 60 分钟有效期、官方文档明确不建议频繁获取，于是脚本就得自己存令牌、
# 判过期、处理并发写。hmacsha1 本地算签名，无状态、无额外往返。
#
# 需要 ORDER_SECRET_ID / ORDER_SECRET_KEY 两个环境变量：getdps 只认**订单**级密钥，
# 拿账户级密钥调它会得到 ERROR(-102): order invalid or expired（签名是过了的，卡在订单查询）。
# 未设置时从当前目录的 .env 读取——ProxyRotation 以项目根目录为工作目录启动时即可直接用。
#
# 代理账号由 f_auth=1 随 IP 一起返回，不必配 proxy_scheme / proxy_user / proxy_pass，
# 也不必额外调一次 getproxyauthorization——账号和 IP 是同一次请求里原子拿到的。
#
# config.yaml 因此只需四行：
#   source: "command"
#   command: "./examples/kuaidaili.sh"
#   mode: "cycle"                              # 别配 loadbalance：每请求换一个 IP 就是每请求买一个 IP
#   test_url: "https://dev.kdlapi.com/testproxy"
set -eu

if [ -z "${ORDER_SECRET_ID:-}" ] && [ -f .env ]; then
	# 不用 `. ./.env`：在 Windows 上编辑过的 .env 是 CRLF，\r 会跟进变量值，
	# 最后表现为 curl 报 "URL using bad/illegal format"，很难往回追。
	eval "$(tr -d '\r' <.env)"
fi
: "${ORDER_SECRET_ID:?ORDER_SECRET_ID is required}"
: "${ORDER_SECRET_KEY:?ORDER_SECRET_KEY is required}"

path="/api/getdps"
# f_auth=1 让结果带上鉴权信息，generateType=4 把它排成 user:pass@ip:port ——
# 前面补个 scheme 就是完整 URL，不用在这里拆字段再拼回去。
# ★generateType 只在带 f_auth 且不带 f_loc / f_citycode / f_et / f_carrier 时有效，
#   要加那几个参数的话这里的输出格式会变，正则也得跟着改。
#
# 签名原文 = 请求方法 + 请求路径 + ? + 按参数名字典序排好的请求字符串。
# 下面这串已经排好序（f_auth < generateType < num < secret_id < sign_type < timestamp），
# 加参数时注意保持。
query="f_auth=1&generateType=4&num=1&secret_id=${ORDER_SECRET_ID}&sign_type=hmacsha1&timestamp=$(date +%s)"
signature=$(
	printf 'GET%s?%s' "$path" "$query" |
		openssl dgst -sha1 -hmac "$ORDER_SECRET_KEY" -binary |
		openssl base64 -A |
		sed -e 's/+/%2B/g' -e 's|/|%2F|g' -e 's/=/%3D/g'
)

# 取一个 IP 就是花一份钱，不重试：失败交给 ProxyRotation 的自愈逻辑决定要不要再来一次。
proxy=$(curl -fsS --max-time 8 "https://dps.kdlapi.com${path}?${query}&signature=${signature}" | tr -d '\r')

# 出错时接口同样返回 200，正文是 "ERROR(-3): 参数错误"，只能按内容判。
# 校验也顺带兜住"格式变了"：宁可退出让自愈决定，也不要拼出一个看似合法的错 URL。
if ! printf '%s' "$proxy" | grep -qE '^[^:@]+:[^:@]+@[0-9]+(\.[0-9]+){3}:[0-9]+$'; then
	printf 'kuaidaili: unexpected response: %s\n' "$proxy" >&2
	exit 1
fi

# ★scheme 硬编码 socks5，别改成 http：同一个 ip:port 上 http 模式会被网关以
#   "460 Proxy Authentication Invalid" 拒掉，socks5 用同一组账号却是通的。
#   写死在这里而不是留成配置项，就是为了让这个结论没法被配错。
printf 'socks5://%s\n' "$proxy"
