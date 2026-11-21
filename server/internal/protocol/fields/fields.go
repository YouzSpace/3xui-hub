// Package fields 定义协议包消费的「表单字段」最小接口。
//
// 为什么独立成包：各协议 settings 构造（protocol/<proto>）需要读取入参表单字段
// （address/port/user_id/…），而表单来源随调用方不同（handler/admin 的 map、
// 将来的其它来源）。用最小接口解耦，避免协议包依赖 admin 的私有类型。
package fields

// Fields 是协议 settings 构造所需的字段读取能力。
// Str 取字符串字段（不存在/非字符串返回空串）；Int 取整数字段（不存在返回 0）。
type Fields interface {
	Str(key string) string
	Int(key string) int64
}
