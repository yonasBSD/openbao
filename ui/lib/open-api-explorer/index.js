/**
 * Copyright (c) HashiCorp, Inc.
 * SPDX-License-Identifier: MPL-2.0
 */

/* eslint-disable ember/avoid-leaking-state-in-ember-objects */
'use strict';

const EngineAddon = require('ember-engines/lib/engine-addon');

module.exports = EngineAddon.extend({
  name: 'open-api-explorer',

  babel: {
    plugins: [require.resolve('ember-auto-import/babel-plugin')],
  },
  included() {
    this._super.included && this._super.included.apply(this, arguments);
    // we want to lazy load the CSS deps, importing them here will result in them being added to the
    // engine-vendor.css files that will be lazy loaded with the engine
    // We DON'T want to add the JS deps here because currently that leads to their inclusion in the vendor.js
    // (this is likely a bug) - to get around that we lazy-load via dynamic `import()` in the swagger-ui.js
    // component
    this.import('node_modules/swagger-ui-dist/swagger-ui.css');
  },

  lazyLoading: {
    enabled: true,
  },

  isDevelopingAddon() {
    return true;
  },
});
