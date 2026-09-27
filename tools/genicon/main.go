// genicon produit les icônes dérivées de internal/icon :
//   - winres/icon.png     ressource de l'exe
//   - installer/app.ico   icône de l'installeur
//   - site/*.png          images du site GitHub Pages
package main

import (
	"image"
	"image/png"
	"log"
	"os"

	"zimbra-smtp-proxy/internal/icon"
)

func writePNG(path string, img image.Image) {
	f, err := os.Create(path)
	if err != nil {
		log.Fatal(err)
	}
	defer f.Close()
	if err := png.Encode(f, img); err != nil {
		log.Fatal(err)
	}
}

func main() {
	writePNG("winres/icon.png", icon.Draw(256, icon.Blue))
	if err := os.WriteFile("installer/app.ico", icon.ICO(icon.Blue, 16, 24, 32, 48, 256), 0o644); err != nil {
		log.Fatal(err)
	}
	writePNG("site/icon.png", icon.Draw(256, icon.Blue))
	writePNG("site/tray-green.png", icon.Draw(64, icon.Green))
	writePNG("site/tray-orange.png", icon.Draw(64, icon.Orange))
	writePNG("site/tray-grey.png", icon.Draw(64, icon.Grey))
}
