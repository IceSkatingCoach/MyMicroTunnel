// SPDX-License-Identifier: GPL-3.0-or-later
package main

import (
	"os"
	"strings"
	"testing"
)

const sampleChangelog = `# Changelog

Some preamble nobody should ship to users.

## 1.0.3

- The menu bar can now remove MyMicroTunnel from this Mac. It explains what it
  does not touch.
- New ` + "`mymicrotunnel diagnose`" + ` collects **everything** a support
  conversation would ask for.

## 1.0.2

- Fixed: the archive held the package one directory down.

## 1.0.0

- First release.
`

func TestChangelogSectionTakesOnlyItsOwnRelease(t *testing.T) {
	section := changelogSection(sampleChangelog, "1.0.3")

	if !strings.Contains(section, "remove MyMicroTunnel") {
		t.Errorf("the section is missing its own content:\n%s", section)
	}
	// The next release's notes appearing in this one's dialog is worse than no
	// notes: it tells the user they are getting something they are not.
	if strings.Contains(section, "one directory down") {
		t.Errorf("the section bled into 1.0.2:\n%s", section)
	}
	if strings.Contains(section, "preamble") {
		t.Errorf("the section picked up the file's preamble:\n%s", section)
	}
}

func TestChangelogSectionForTheOldestEntry(t *testing.T) {
	section := changelogSection(sampleChangelog, "1.0.0")
	if section != "- First release." {
		t.Errorf("got %q", section)
	}
}

func TestChangelogSectionForAVersionThatIsNotThere(t *testing.T) {
	// Shipping an update that says nothing about itself is bad; shipping one
	// that silently carries the previous release's notes is worse.
	if section := changelogSection(sampleChangelog, "9.9.9"); section != "" {
		t.Errorf("got %q, want nothing", section)
	}
}

func TestMarkdownToHTMLRendersWhatTheNotesActuallyUse(t *testing.T) {
	html := markdownToHTML(changelogSection(sampleChangelog, "1.0.3"))

	for _, expected := range []string{
		"<ul>", "</ul>", "<li>",
		"<code>mymicrotunnel diagnose</code>",
		"<strong>everything</strong>",
	} {
		if !strings.Contains(html, expected) {
			t.Errorf("the rendered notes have no %q:\n%s", expected, html)
		}
	}

	// A wrapped bullet is one bullet. Treating the continuation as its own
	// paragraph produces a dialog full of orphaned sentence fragments.
	if strings.Count(html, "<li>") != 2 {
		t.Errorf("got %d list items, want 2:\n%s", strings.Count(html, "<li>"), html)
	}
	if !strings.Contains(html, "It explains what it does not touch.") {
		t.Errorf("a wrapped bullet was split:\n%s", html)
	}
}

func TestMarkdownToHTMLEscapesWhatWouldBreakTheFeed(t *testing.T) {
	// The notes are embedded in XML. An unescaped angle bracket makes a feed
	// that every installed copy fails to parse.
	html := markdownToHTML("- a <script> tag & an ampersand")

	if strings.Contains(html, "<script>") {
		t.Errorf("a tag survived unescaped:\n%s", html)
	}
	if !strings.Contains(html, "&lt;script&gt;") || !strings.Contains(html, "&amp;") {
		t.Errorf("escaping did not happen:\n%s", html)
	}
}

// The notes shipped to users come from this repository's own CHANGELOG.md, so
// the release being cut had better have an entry in it.
func TestThisReleaseHasNotes(t *testing.T) {
	changelog, err := os.ReadFile("../../CHANGELOG.md")
	if err != nil {
		t.Skip("no CHANGELOG.md")
	}
	version, err := os.ReadFile("../../VERSION")
	if err != nil {
		t.Skip("no VERSION")
	}

	current := strings.TrimSpace(string(version))
	if section := changelogSection(string(changelog), current); section == "" {
		t.Errorf("CHANGELOG.md has no '## %s' section; that release would ship "+
			"an update dialog that says nothing about itself", current)
	}
}
