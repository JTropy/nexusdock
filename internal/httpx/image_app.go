package httpx

import _ "embed"

const imageAppURI = "ui://nexusdock/view-image/v1.html"

//go:embed image_app.html
var imageAppHTML string
