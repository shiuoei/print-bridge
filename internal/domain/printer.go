package domain

// PrinterInfo is the capability snapshot the agent returns for
// `get_printer_info` (wire type `printer_info`). The field names and JSON tags
// mirror the protocol's AgentPrinterInfo/AgentPrinterPaper/AgentPrinterTray/
// AgentPrinterMediaType so the raw struct can be serialized onto the wire
// unchanged.
//
// Optional members are pointers because "unknown" is not the same as "false" or
// "empty": a backend that cannot prove a value must leave it nil instead of
// inventing one. Papers/Trays/MediaTypes are always non-nil slices so they
// encode as `[]` rather than `null`.
type PrinterInfo struct {
	Name       string             `json:"name"`
	IsDefault  *bool              `json:"is_default,omitempty"`
	DPI        *int               `json:"dpi,omitempty"`
	Port       *string            `json:"port,omitempty"`
	IsLocal    *bool              `json:"is_local,omitempty"`
	IsNetwork  *bool              `json:"is_network,omitempty"`
	IsVirtual  *bool              `json:"is_virtual,omitempty"`
	Papers     []PrinterPaper     `json:"papers"`
	Trays      []PrinterTray      `json:"trays"`
	MediaTypes []PrinterMediaType `json:"media_types"`
}

// PrinterPaper is one selectable paper size reported by the printer driver.
// WidthMM/HeightMM are millimetres; a backend that cannot size a paper must omit
// that paper rather than report a zero size.
type PrinterPaper struct {
	ID       string  `json:"id,omitempty"`
	Name     string  `json:"name,omitempty"`
	WidthMM  float64 `json:"width_mm"`
	HeightMM float64 `json:"height_mm"`
}

// PrinterTray is one input slot / paper source.
type PrinterTray struct {
	ID   string `json:"id"`
	Name string `json:"name"`
}

// PrinterMediaType is one supported media type (plain paper, label, photo...).
type PrinterMediaType struct {
	ID   string `json:"id"`
	Name string `json:"name"`
}
