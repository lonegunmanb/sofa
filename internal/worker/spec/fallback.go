package spec

type FallbackConfig struct {
	Name       string
	On         []string
	Target     string
	MaxReworks int
	Artifacts  []string
}

type FallbackBlock struct {
	Name       string
	On         []string
	Target     string
	MaxReworks int
	Artifacts  []string
}
