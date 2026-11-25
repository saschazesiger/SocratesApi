/*
 * Settings: remote access through a Cloudflare tunnel and the admin password, both drawn by the Z-Dash server's
 * shell (mountTunnel, mountPassword), and the version. The API's own settings (keys required, the Hugging Face token)
 * stay on the API page, next to the keys.
 */
import { mountTunnel } from '/z-dash/server/tunnel.js';
import { mountPassword } from '/z-dash/server/password.js';
import { ctx } from './main.js';
import { $, esc, head } from './ui.js';

/* The shell's parts hold their own state and follow their own notices; these keep them while the view shows. */
let tunnel = null;
let password = null;

function unmount() {
  if (tunnel) tunnel.destroy();
  if (password) password.destroy();
  tunnel = null;
  password = null;
}

export default {
  title: 'Settings',

  load: async () => ({}),

  html() {
    const s = ctx.session || {};
    const who = s.open ? 'Sign-in is off' : s.via === 'tunnel' ? `${s.email}, through Cloudflare Access` : 'Admin, with the password';
    return head('Settings', 'How Socrates API is reached from outside and how you sign in.') +
      '<div class="ds-stack">' +
        '<section class="ds-card" aria-labelledby="tunnel-title"><h2 class="ds-card__title" id="tunnel-title">Remote access</h2>' +
          '<p class="ds-muted">Open this page from anywhere through a Cloudflare tunnel: no open port, no router setup. The tunnel, its address and the login are created for you; through the tunnel, Cloudflare Access signs people in by their e-mail address. The local address and its password keep working either way. The model API stays on your network: through the tunnel it needs the Cloudflare login too.</p>' +
          '<div id="so-tunnel"></div></section>' +
        '<section class="ds-card" aria-labelledby="password-title"><h2 class="ds-card__title" id="password-title">Admin password</h2>' +
          '<div id="so-password"></div></section>' +
        '<section class="ds-card" aria-labelledby="about-title"><h2 class="ds-card__title" id="about-title">About</h2>' +
          '<ul class="ds-kv-list">' +
            `<li><span>Version</span><strong>${esc(s.version || 'dev')}</strong></li>` +
            `<li><span>Signed in</span><strong>${esc(who)}</strong></li>` +
          '</ul></section>' +
      '</div>';
  },

  mount() {
    unmount();
    tunnel = mountTunnel($('#so-tunnel'), { name: (ctx.session && ctx.session.name) || 'Socrates API', level: 3 });
    password = mountPassword($('#so-password'), ctx.session);
  },

  leave() {
    unmount();
  },
};
