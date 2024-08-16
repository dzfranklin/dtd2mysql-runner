package clips

import (
	"embed"
	"strings"
)

//go:embed *.json
var embedded embed.FS

func Get() map[string]string {
	entries, err := embedded.ReadDir(".")
	if err != nil {
		panic(err)
	}
	out := make(map[string]string)
	for _, entry := range entries {
		contents, err := embedded.ReadFile(entry.Name())
		if err != nil {
			panic(err)
		}
		out[strings.TrimSuffix(entry.Name(), ".json")] = string(contents)
	}
	return out
}
