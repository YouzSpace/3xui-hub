package admin

// 本文件保留管理端各域共用的 SQL 参数转换辅助：
// nullable* 把 Go 的可选值转成 database/sql 可接受的 NULL/值，
// phpFloatOf 复刻 PHP 浮点序列化口径（与 phpjson.go 的 phpFloat 配套）。
// 代理商城（proxy-store / proxy-ops / upstream-webshare）已下线，
// 原文件里的商品/规格 Handler 一并移除。

// ptrStringOrNil 把输入值转成 *string（null → nil，其余按 PHP (string)）。
func ptrStringOrNil(v any) *string {
	if v == nil {
		return nil
	}
	s, ok := inputString(v)
	if !ok {
		return nil
	}
	return &s
}

// nullableUint 输出 uint64 指针（nil → null）。
func nullableUint(v *uint64) any {
	if v == nil {
		return nil
	}
	return *v
}

// nullableInt 输出 int64 指针（nil → null）。
func nullableInt(v *int64) any {
	if v == nil {
		return nil
	}
	return *v
}

// phpFloatOf 把 decimal 字符串按 PHP 的 (float) 输出。
//
// decimal 只有 2 位小数，float64 表达无损；输出用最短往返表示，与 PHP 的
// json_encode((float)"10.00") → 10 一致。
func phpFloatOf(decimal string) phpFloat {
	return phpFloat(parseFloatSafe(decimal))
}

// derefString 解引用字符串指针（nil → 空串）。
func derefString(s *string) string {
	if s == nil {
		return ""
	}
	return *s
}

// phpStrToUpper 对齐 PHP strtoupper()：只转 ASCII a-z（不碰多字节）。
func phpStrToUpper(s string) string {
	b := []byte(s)
	for i := 0; i < len(b); i++ {
		if b[i] >= 'a' && b[i] <= 'z' {
			b[i] -= 'a' - 'A'
		}
	}
	return string(b)
}
