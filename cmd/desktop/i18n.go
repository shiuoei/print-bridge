package main

import (
	"embed"
	"encoding/json"
	"fmt"
)

//go:embed locales/en.json locales/zh-CN.json
var localeFiles embed.FS

var catalogs = loadCatalogs()

func loadCatalogs() map[string]map[string]string {
	out := make(map[string]map[string]string, 2)
	for _, language := range []string{"en", "zh-CN"} {
		data, err := localeFiles.ReadFile("locales/" + language + ".json")
		if err != nil {
			panic(fmt.Sprintf("read %s locale: %v", language, err))
		}
		var entries map[string]string
		if err := json.Unmarshal(data, &entries); err != nil {
			panic(fmt.Sprintf("parse %s locale: %v", language, err))
		}
		out[language] = entries
	}
	return out
}

func (d *desktop) tr(key string) string {
	if value := catalogs[d.language][key]; value != "" {
		return value
	}
	if value := catalogs["en"][key]; value != "" {
		return value
	}
	return key
}
