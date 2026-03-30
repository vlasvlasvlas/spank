#!/bin/bash

# Script interactivo para ejecutar spank en diferentes modos

spank_bin="./spank"
if ! [ -x "$spank_bin" ]; then
    spank_bin="spank"
fi

function show_menu() {
    clear
    echo "==============================="
    echo "         SPANK MENU            "
    echo "==============================="
    echo "1) Modo normal (pain)"
    echo "2) Modo sexy"
    echo "3) Modo halo"
    echo "4) Modo rápido (fast)"
    echo "5) Modo sexy + rápido"
    echo "6) Modo custom (personalizado)"
    echo "x) Salir"
    echo "==============================="
    echo -n "Elige una opción: "
}

while true; do
    show_menu
    read -r opcion
    case $opcion in
        1)
            sudo $spank_bin
            ;;
        2)
            sudo $spank_bin --sexy
            ;;
        3)
            sudo $spank_bin --halo
            ;;
        4)
            sudo $spank_bin --fast
            ;;
        5)
            sudo $spank_bin --sexy --fast
            ;;
        6)
            echo -n "Ruta al directorio de MP3 personalizados: "
            read -r custom_dir
            sudo $spank_bin --custom "$custom_dir"
            ;;
        x|X)
            echo "¡Hasta luego!"
            exit 0
            ;;
        *)
            echo "Opción no válida. Pulsa enter para continuar."
            read -r
            ;;
    esac
done
