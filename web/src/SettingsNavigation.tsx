export function SettingsNavigation({
  active,
}: {
  active: "tokens" | "ssh-keys";
}) {
  return (
    <nav aria-label="Personal settings">
      {[
        ["tokens", "Access tokens"],
        ["ssh-keys", "SSH keys"],
      ].map(([path, label]) => (
        <a
          key={path}
          className={`side-link${active === path ? " active" : ""}`}
          href={`/auth/${path}`}
          aria-current={active === path ? "page" : undefined}
        >
          {label}
        </a>
      ))}
    </nav>
  );
}
