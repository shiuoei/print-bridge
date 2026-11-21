package main

import "testing"

func TestLocaleCatalogsHaveMatchingKeys(t *testing.T) {
	en, zh := catalogs["en"], catalogs["zh-CN"]
	if len(en) == 0 || len(zh) == 0 {
		t.Fatal("locale catalog is empty")
	}
	for key, value := range en {
		if value == "" || zh[key] == "" {
			t.Errorf("missing translation for %q", key)
		}
	}
	for key := range zh {
		if _, ok := en[key]; !ok {
			t.Errorf("Chinese catalog has unknown key %q", key)
		}
	}
}
