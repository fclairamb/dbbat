module.exports = {
  extends: ['@commitlint/config-conventional'],
  rules: {
    'scope-enum': [
      2,
      'always',
      [
        'api',
        'auth',
        'config',
        'crypto',
        'db',
        'deps',
        'docs',
        'grants',
        'migrations',
        'proxy',
        'store',
        'ui',
        'release'
      ]
    ],
    'scope-empty': [1, 'never'],  // Warning if scope is missing
    'body-max-line-length': [0],  // Disable body line length limit
  }
};
