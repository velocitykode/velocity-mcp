package ui

// Library is a front-end library an app resource opts into. The SDK injects the
// library's script tags into the served HTML and adds its CDN host to the app's
// Content Security Policy resourceDomains, so the document loads without the
// author hand-writing either.
type Library string

const (
	// LibraryTailwind loads the Tailwind CSS browser build.
	LibraryTailwind Library = "tailwind"
	// LibraryAlpine loads Alpine.js for lightweight interactivity.
	LibraryAlpine Library = "alpine"
)

// Domains returns the CSP resourceDomains the library must be allowed to load
// from. They are merged into the app's Content Security Policy automatically.
func (l Library) Domains() []string {
	switch l {
	case LibraryTailwind:
		return []string{"https://cdn.tailwindcss.com"}
	case LibraryAlpine:
		return []string{"https://cdn.jsdelivr.net"}
	default:
		return nil
	}
}

// ScriptTags returns the HTML tags to inject into the document head to load the
// library. An unknown library yields no tags.
func (l Library) ScriptTags() []string {
	switch l {
	case LibraryTailwind:
		return []string{
			`<script src="https://cdn.tailwindcss.com"></script>`,
			`<script>tailwind.config = { darkMode: ['selector', '[data-theme="dark"]'] }</script>`,
		}
	case LibraryAlpine:
		return []string{
			`<style>[x-cloak] { display: none !important; }</style>`,
			`<script defer src="https://cdn.jsdelivr.net/npm/alpinejs@3/dist/cdn.min.js"></script>`,
		}
	default:
		return nil
	}
}
