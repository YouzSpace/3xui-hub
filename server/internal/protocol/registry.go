package protocol

// 协议注册表与分发见 fields.go 的 Registry/Register/Lookup/BuildOutboundSettings。
// 各协议子包在 init() 中 protocol.Register(Spec{...})，消费方空导入子包即可。
