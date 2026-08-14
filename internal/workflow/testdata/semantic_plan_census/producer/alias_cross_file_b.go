package producer

type crossFileOther = crossFileAlias

func AliasCrossFile() {
	_ = crossFileOther{"", "", "", "", "", 0, "", "", true, "@smol", nil, nil, "", "", "", nil}
}
