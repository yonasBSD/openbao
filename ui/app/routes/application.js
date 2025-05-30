/**
 * Copyright (c) HashiCorp, Inc.
 * SPDX-License-Identifier: MPL-2.0
 */

import { inject as service } from '@ember/service';
import Route from '@ember/routing/route';
import ControlGroupError from 'vault/lib/control-group-error';

export default Route.extend({
  controlGroup: service(),
  routing: service('router'),
  namespaceService: service('namespace'),
  featureFlagService: service('featureFlag'),

  actions: {
    willTransition() {
      window.scrollTo(0, 0);
    },
    error(error, transition) {
      const controlGroup = this.controlGroup;
      if (error instanceof ControlGroupError) {
        return controlGroup.handleError(error);
      }
      if (error.path === '/v1/sys/wrapping/unwrap') {
        controlGroup.unmarkTokenForUnwrap();
      }

      const router = this.routing;
      //FIXME transition.intent likely needs to be replaced
      let errorURL = transition.intent.url;
      const { name, contexts, queryParams } = transition.intent;

      // If the transition is internal to Ember, we need to generate the URL
      // from the route parameters ourselves
      if (!errorURL) {
        try {
          errorURL = router.urlFor(name, ...(contexts || []), { queryParams });
        } catch {
          // If this fails, something weird is happening with URL transitions
          errorURL = null;
        }
      }
      // because we're using rootURL, we need to trim this from the front to get
      // the ember-routeable url
      if (errorURL) {
        errorURL = errorURL.replace('/ui', '');
      }

      error.errorURL = errorURL;

      // if we have queryParams, update the namespace so that the observer can fire on the controller
      if (queryParams) {
        /* eslint-disable-next-line ember/no-controller-access-in-routes */
        this.controllerFor('vault.cluster').set('namespaceQueryParam', queryParams.namespace || '');
      }

      // Assuming we have a URL, push it into browser history and update the
      // location bar for the user
      if (errorURL) {
        router.get('location').setURL(errorURL);
      }

      return true;
    },
  },

  async beforeModel() {
    const result = await fetch('/v1/sys/internal/ui/feature-flags', {
      method: 'GET',
    });
    if (result.status === 200) {
      const body = await result.json();
      const flags = body.feature_flags || [];
      this.featureFlagService.setFeatureFlags(flags);
    }
  },
});
