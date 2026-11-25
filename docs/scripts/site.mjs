// Where the site and its repository live. GitHub Actions sets GITHUB_REPOSITORY, so a
// build in a fork links to the fork. A local build uses the default below.
const slug = process.env.GITHUB_REPOSITORY || 'platform-kit-org/metal-operator';

/** The repository on GitHub, for example "https://github.com/platform-kit-org/metal-operator". */
export const repoUrl = `https://github.com/${slug}`;
/**
 * The host of the site: the custom domain in the Pages settings of the repository.
 * A fork without a custom domain sets PK_DOCS_SITE=https://<owner>.github.io and
 * PK_DOCS_BASE=/<repo>.
 */
export const site = process.env.PK_DOCS_SITE || 'https://metal.platform-kit.org';
/** The path of the site on the host, without a trailing slash: "" for the root. */
export const base = (process.env.PK_DOCS_BASE || '').replace(/\/$/, '');
/** The branch that the site documents and deploys from. */
export const branch = 'main';
