// A copy button on every command block.
//
// The page works without this: the script only adds buttons, so a reader with
// JavaScript switched off still sees, and can select, every command.
//
// Response bodies are marked up as .out and left out of what gets copied.
// Copying the answer along with the question is how people end up pasting
// JSON into a shell.

for (const block of document.querySelectorAll("pre")) {
  const code = block.querySelector("code");
  if (!code) continue;

  const text = () => {
    const copy = code.cloneNode(true);
    for (const out of copy.querySelectorAll(".out")) out.remove();
    return copy.textContent.trim();
  };
  if (!text()) continue; // nothing but output

  const button = document.createElement("button");
  button.type = "button";
  button.className = "copy";
  button.textContent = "Copy";
  button.setAttribute("aria-label", "Copy to clipboard");

  button.addEventListener("click", async () => {
    try {
      await navigator.clipboard.writeText(text());
      button.textContent = "Copied";
    } catch {
      button.textContent = "Press ⌘C";
    }
    setTimeout(() => {
      button.textContent = "Copy";
    }, 1600);
  });

  block.appendChild(button);
}
