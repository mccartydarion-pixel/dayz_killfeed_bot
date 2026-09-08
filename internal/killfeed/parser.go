package killfeed

// Parser defines the contract for incremental log parsing.
type Parser interface {
	ParseLine(line string) (*Event, error)
}

// PlaceholderParser is intentionally incomplete until real DayZ log samples are available.
type PlaceholderParser struct{}

// ParseLine is a stub and should be implemented with real DayZ log handling in Phase 2.
func (p *PlaceholderParser) ParseLine(line string) (*Event, error) {
	_ = line
	return nil, nil
}
