package upstream

// Realm 是账号所属区域。国内版与国际版是两套完全独立的账号体系：
// 域名不同、模型目录不同、计费不同、登录入口不同。
//
// 混用会出问题：拿国内账号去请求国际端点会被拒，反之亦然。
// 因此区域不是"配置项"而是"账号的第一等属性"，一路带到选号、
// 端点解析、模型表、面板展示。
type Realm struct {
	Key   string // "cn" / "intl"
	Name  string // 中文显示名
	Short string // 徽标上的短名
}

// 全部支持区域。顺序即面板里的展示顺序。
var Realms = []Realm{
	{Key: "cn", Name: "国内版", Short: "CN"},
	{Key: "intl", Name: "国际版", Short: "INTL"},
}

// RealmByKey 查区域定义，未知值回落到国内版。
func RealmByKey(key string) Realm {
	for _, r := range Realms {
		if r.Key == key {
			return r
		}
	}
	return Realms[0]
}

// ValidRealm 判断是否是受支持的区域。
func ValidRealm(key string) bool {
	return key == "cn" || key == "intl"
}

// 端点常量。
//
// 两套区域的核心差异全在这里：
//   - 国内版：聊天与 CLI 域在 copilot.tencent.com，签到积分在 www.codebuddy.cn
//   - 国际版：全部在 www.workbuddy.ai，且没有签到/成长任务
//
// 注意 www.workbuddy.ai 与 www.codebuddy.cn 是不同产品线，
// www.codebuddy.cn 上还有 www.workbuddy.cn（国内 Web 端），领奖必须走后者。
const (
	// CNChatBase 国内版聊天与 CLI 域
	CNChatBase = "https://copilot.tencent.com"
	// CNBillingBase 国内版签到 / 积分域
	CNBillingBase = "https://www.codebuddy.cn"
	// CNWebBase 国内 Web 域（领奖必须走这里，走 CLI 域会 200 但不记账）
	CNWebBase = "https://www.workbuddy.cn"

	// IntlChatBase 国际版统一入口（聊天 / 签到 / 积分都在这一个域）
	IntlChatBase = "https://www.workbuddy.ai"
)

// ChatBaseFor 返回指定区域的聊天端点。
func ChatBaseFor(realm string) string {
	if realm == "intl" {
		return IntlChatBase
	}
	return CNChatBase
}

// BillingBaseFor 返回指定区域的签到 / 积分端点。
func BillingBaseFor(realm string) string {
	if realm == "intl" {
		return IntlChatBase
	}
	return CNBillingBase
}

// HasGrowthTasks 报告该区域是否有签到与成长任务。
//
// 国际版没有这套体系（国际站是订阅制，权益走每日活跃对话而非签到），
// 所以定时任务里的签到 / 旅行 / 成长任务对国际账号全部跳过。
func HasGrowthTasks(realm string) bool { return realm != "intl" }

// HasCheckin 报告该区域是否支持每日签到领积分。
func HasCheckin(realm string) bool { return realm == "cn" }

// DomainsFor 返回该区域涉及的所有域名，用于面板展示与诊断。
func DomainsFor(realm string) []string {
	if realm == "intl" {
		return []string{"www.workbuddy.ai"}
	}
	return []string{"copilot.tencent.com", "www.codebuddy.cn", "www.workbuddy.cn"}
}
