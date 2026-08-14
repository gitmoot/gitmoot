package producer

type crossFileOther = crossFileAlias
type crossFileUnrelated struct{ Plan bool }

func AliasCrossFile() {
	_ = crossFileOther{PlanInto: "@smol"}
}

func AliasCrossFileControl() {
	_ = crossFileUnrelated{Plan: true}
}
