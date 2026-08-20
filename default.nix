{ lib
, buildGoApplication
, nix-gitignore
, makeWrapper
, wl-mirror
, wlr-randr
}:

buildGoApplication (lib.fix (finalAttrs: {
  pname = "mangomon";
  version = lib.fileContents ./version.txt;

  src = nix-gitignore.gitignoreSource [ ] ./.;

  modules = ./gomod2nix.toml;

  ldflags = [
    "-s"
    "-w"
    "-X main.Version=${finalAttrs.version}"
  ];

  nativeBuildInputs = [ makeWrapper ];

  postFixup = ''
    wrapProgram $out/bin/mangomon --prefix PATH : "${lib.makeBinPath [ wl-mirror wlr-randr ]}"
  '';

  meta = {
    description = "tui monitor configuration tool for mango with visual layout, drag-and-drop, and profile management";
    homepage = "https://github.com/stepbrobd/mangomon";
    license = lib.licenses.asl20;
    mainProgram = "mangomon";
    maintainers = with lib.maintainers; [ stepbrobd ];
    platforms = lib.platforms.linux;
  };
}))
