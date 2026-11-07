// Copyright 2025
// SPDX-License-Identifier: Apache-2.0

package tui

import (
	"github.com/charmbracelet/lipgloss"
)

// Color palette
var (
	ColorPrimary   = lipgloss.Color("86")  // Cyan
	ColorSecondary = lipgloss.Color("99")  // Purple
	ColorSuccess   = lipgloss.Color("82")  // Green
	ColorWarning   = lipgloss.Color("214") // Orange
	ColorMuted     = lipgloss.Color("241") // Gray
	ColorHighlight = lipgloss.Color("229") // Yellow

	ColorBorder = lipgloss.Color("238")
)

// Selector styles
var (
	// Header styles

	TitleStyle = lipgloss.NewStyle().
			Bold(true).
			Foreground(ColorPrimary).
			MarginBottom(1)

	SubtitleStyle = lipgloss.NewStyle().
			Foreground(ColorMuted)

	HeaderInfoStyle = lipgloss.NewStyle().
			Foreground(ColorSecondary)

	// Item styles

	ItemStyle = lipgloss.NewStyle().
			PaddingLeft(2)

	SelectedItemStyle = lipgloss.NewStyle().
				PaddingLeft(2).
				Foreground(ColorSuccess)

	CursorStyle = lipgloss.NewStyle().
			Foreground(ColorPrimary).
			Bold(true)

	// CheckboxChecked and CheckboxUnchecked render a selected and an unselected checkbox.
	CheckboxChecked   = lipgloss.NewStyle().Foreground(ColorSuccess).SetString("[x]")
	CheckboxUnchecked = lipgloss.NewStyle().Foreground(ColorMuted).SetString("[ ]")

	// StarFilled and StarEmpty render the filled and empty quality stars.
	StarFilled = lipgloss.NewStyle().Foreground(ColorWarning).SetString("★")
	StarEmpty  = lipgloss.NewStyle().Foreground(ColorMuted).SetString("☆")

	// Labels

	RecommendedBadge = lipgloss.NewStyle().
				Foreground(lipgloss.Color("0")).
				Background(ColorSuccess).
				Padding(0, 1).
				SetString("recommended")

	SizeLabelStyle = lipgloss.NewStyle().
			Foreground(ColorMuted).
			Width(10).
			Align(lipgloss.Right)

	RAMLabelStyle = lipgloss.NewStyle().
			Foreground(ColorMuted).
			Width(12)

	DescriptionStyle = lipgloss.NewStyle().
				Foreground(ColorMuted).
				Italic(true)

	// CategoryStyle styles a category header.
	CategoryStyle = lipgloss.NewStyle().
			Bold(true).
			Foreground(ColorSecondary).
			MarginTop(1).
			MarginBottom(0)

	// FooterStyle styles the key-hint footer.
	FooterStyle = lipgloss.NewStyle().
			Foreground(ColorMuted).
			MarginTop(1)

	// Command box

	CommandBoxStyle = lipgloss.NewStyle().
			Border(lipgloss.RoundedBorder()).
			BorderForeground(ColorBorder).
			Padding(0, 1).
			MarginTop(1)

	CommandLabelStyle = lipgloss.NewStyle().
				Foreground(ColorMuted).
				Bold(true)

	CommandTextStyle = lipgloss.NewStyle().
				Foreground(ColorHighlight)

	// Summary styles

	SummaryLabelStyle = lipgloss.NewStyle().
				Foreground(ColorMuted)

	SummaryValueStyle = lipgloss.NewStyle().
				Foreground(ColorPrimary).
				Bold(true)

	// Help keys

	HelpStyle = lipgloss.NewStyle().
			Foreground(ColorMuted)

	HelpKeyStyle = lipgloss.NewStyle().
			Foreground(ColorPrimary)

	// SuccessStyle styles a success marker.
	SuccessStyle = lipgloss.NewStyle().
			Foreground(ColorSuccess).
			Bold(true)
)

// RenderStars renders quality stars (filled and empty).
func RenderStars(quality int) string {
	if quality <= 0 {
		return ""
	}
	if quality > 5 {
		quality = 5
	}

	var s string
	for i := 0; i < quality; i++ {
		s += StarFilled.String()
	}
	for i := quality; i < 5; i++ {
		s += StarEmpty.String()
	}
	return s
}

// RenderCheckbox renders a checkbox based on checked state.
func RenderCheckbox(checked bool) string {
	if checked {
		return CheckboxChecked.String()
	}
	return CheckboxUnchecked.String()
}

// FormatCategoryTitle formats a category key into a display title.
func FormatCategoryTitle(category string) string {
	switch category {
	case "quantization":
		return "Quantizations"
	case "variant":
		return "Precision Variants"
	case "component":
		return "Components"
	case "split":
		return "Dataset Splits"
	case "format":
		return "Weight Format"
	case "precision":
		return "Precision"
	default:
		return "Options"
	}
}
