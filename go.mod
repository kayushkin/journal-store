module github.com/kayushkin/journal-store

go 1.26.0

require (
	github.com/kayushkin/llm-bridge v0.0.0
	github.com/kayushkin/people-store v0.0.0
	github.com/kayushkin/photo-store v0.0.0
	github.com/mattn/go-sqlite3 v1.14.37
)

require golang.org/x/image v0.47.0 // indirect

replace github.com/kayushkin/llm-bridge => ../llm-bridge

replace github.com/kayushkin/people-store => ../people-store

replace github.com/kayushkin/photo-store => ../photo-store
