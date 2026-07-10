package main

import "embed"

//go:embed web/admin/*
var adminStaticFS embed.FS

//go:embed web/user/*
var userStaticFS embed.FS
