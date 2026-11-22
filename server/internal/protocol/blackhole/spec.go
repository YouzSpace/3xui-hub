package blackhole

import p "controlhub-server/internal/protocol"

func init() { p.Register(p.Spec{Name: "blackhole"}) }
