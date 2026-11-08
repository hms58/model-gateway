package dataplane

import "encoding/json"

// streamRequest 只关心是否需要流式，其余字段原样透传。
// 流式与非流式共用同一条降级主链（见 relay），这里只做请求投影。
func streamRequest(body []byte) bool {
	var head struct {
		Stream bool `json:"stream"`
	}
	if err := json.Unmarshal(body, &head); err != nil {
		return false
	}
	return head.Stream
}
