// SPDX-License-Identifier: GPL-3.0-or-later

package client

// splitTunnelDomainSuffixes is the built-in direct-routing set used when the
// desktop's split-tunnel option is enabled. The list deliberately starts with
// country-code and local-network suffixes, then covers popular mainland-China
// services whose public names use generic TLDs. Keeping the data in the client
// means a connection never depends on downloading a remote rule-set before the
// relay is usable.
//
// Values use sing-box's domain_suffix semantics: a bare registrable domain
// matches both the apex and its subdomains, while the leading-dot country-code
// entry matches every name below that TLD.
var splitTunnelDomainSuffixes = []string{
	".cn",
	".lan",
	".local",
	"1688.com",
	"58.com",
	"alibaba.com",
	"alicdn.com",
	"alipay.com",
	"aliyun.com",
	"amap.com",
	"autohome.com",
	"autonav.com",
	"baidu.com",
	"bcebos.com",
	"bdstatic.com",
	"bilibili.com",
	"bilivideo.com",
	"bytedance.com",
	"byteimg.com",
	"csdn.net",
	"ctrip.com",
	"dianping.com",
	"dingtalk.com",
	"douban.com",
	"douyin.com",
	"douyu.com",
	"eastmoney.com",
	"ele.me",
	"ganji.com",
	"gitee.com",
	"gtimg.com",
	"hdslb.com",
	"honor.com",
	"huawei.com",
	"huya.com",
	"iqiyi.com",
	"ixigua.com",
	"jd.com",
	"jdcache.com",
	"jdcloud.com",
	"kingsoft.com",
	"kuaishou.com",
	"kwimgs.com",
	"meituan.com",
	"mi.com",
	"miui.com",
	"myqcloud.com",
	"netease.com",
	"oppo.com",
	"pinduoduo.com",
	"pstatp.com",
	"qhimg.com",
	"qiyi.com",
	"qq.com",
	"qunar.com",
	"sina.com",
	"smzdm.com",
	"so.com",
	"sogou.com",
	"suning.com",
	"taobao.com",
	"tencent.com",
	"tmall.com",
	"toutiao.com",
	"tudou.com",
	"vivo.com",
	"vip.com",
	"wechat.com",
	"weibo.com",
	"wps.com",
	"xhscdn.com",
	"xiaohongshu.com",
	"xueqiu.com",
	"youku.com",
	"zhihu.com",
	"zhimg.com",
}

func buildDNSConfig(servers []string, splitTunnel bool) map[string]any {
	configured := dnsServerObjects(servers)
	dns := map[string]any{
		"servers": configured,
		"final":   "dns-0",
	}
	if !splitTunnel {
		return dns
	}

	// Foreign lookups keep using dns-0 through the relay. Direct destinations
	// use the platform resolver so they receive the same nearby/CDN answers as
	// other applications on the local network.
	dns["servers"] = append(configured, map[string]any{
		"type": "local",
		"tag":  "dns-direct",
	})
	dns["rules"] = []any{
		map[string]any{
			"domain_suffix": splitTunnelDomainSuffixes,
			"action":        "route",
			"server":        "dns-direct",
		},
	}
	dns["reverse_mapping"] = true
	return dns
}

func buildRouteConfig(mode InboundMode, splitTunnel bool) map[string]any {
	rules := []any{
		map[string]any{
			"protocol": "dns",
			"action":   "hijack-dns",
		},
	}
	if splitTunnel {
		// Private addresses should never take a detour through a public relay.
		rules = append(rules, map[string]any{
			"ip_is_private": true,
			"action":        "route",
			"outbound":      "direct",
		})
		// A mixed HTTP/SOCKS inbound already carries the requested hostname. TUN
		// traffic generally arrives as an IP connection, so sniff only that
		// inbound before applying the same domain rule.
		if mode == ModeTUN {
			rules = append(rules, map[string]any{
				"inbound": "tun-in",
				"action":  "sniff",
			})
		}
		rules = append(rules, map[string]any{
			"domain_suffix": splitTunnelDomainSuffixes,
			"action":        "route",
			"outbound":      "direct",
		})
	}

	return map[string]any{
		"auto_detect_interface":   true,
		"default_domain_resolver": "dns-0",
		"rules":                   rules,
		"final":                   "proxy",
	}
}
